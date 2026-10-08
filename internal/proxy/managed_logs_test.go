package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const logTestKey = "pst-private-log-client-key-0123456789"
const logTestPrompt = "private-prompt-never-store-this"

func newLogTestHandler(t *testing.T, state string) (*ManagedHandler, *atomic.Int32) {
	t.Helper()
	if state == "" {
		state = filepath.Join(t.TempDir(), "keys.json")
	}
	var calls atomic.Int32
	image := archiveTestPNG(color.RGBA{R: 255, A: 255})
	transport := settlementTransport(func(r *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1}
		if r.URL.Path == "/user/subscription" {
			response.Body = io.NopCloser(strings.NewReader(`{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":10000,"purchasedTrainingSteps":0},"usage":{"percent":100,"isNegative":false}}`))
			return response, nil
		}
		calls.Add(1)
		message := "Not enough Anlas. Required: 18. " + testNAIToken + " " + logTestKey + " " + logTestPrompt + " private-negative-prompt private-character-caption " + strings.Repeat("a", 120)
		switch r.Header.Get("X-Correlation-Id") {
		case "http-error":
			response.StatusCode = 400
			response.Header.Set("Content-Type", "application/json")
			data, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
			response.Body = io.NopCloser(bytes.NewReader(data))
		case "stream-error":
			response.Body = io.NopCloser(bytes.NewReader(archiveTestFrame(map[string]any{"code": 402, "message": message})))
		case "uncertain":
			response.StatusCode = 502
			response.Body = io.NopCloser(strings.NewReader("upstream disconnected"))
		default:
			data := image
			response.Header.Set("Content-Type", "image/png")
			if strings.HasSuffix(r.URL.Path, "generate-image-stream") {
				data = archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": image})
				response.Header.Set("Content-Type", "application/octet-stream")
			}
			response.Body = io.NopCloser(bytes.NewReader(data))
		}
		return response, nil
	})
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: state, ImageUpstream: "http://upstream.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginDrain)
	cipher, _ := h.vault.seal(logTestKey)
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "log-device", Name: "device", Hash: hexHash(logTestKey), KeyCiphertext: cipher, PolicyVersion: 2, AllowFixed: true, FixedLimit: 5000}), nil
	}); err != nil {
		t.Fatal(err)
	}
	return h, &calls
}

func logTestRequest(t *testing.T, h *ManagedHandler, path, key string, body any, correlation string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Correlation-Id", correlation)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRequestLogsCaptureErrorsAndProtectSecrets(t *testing.T) {
	h, calls := newLogTestHandler(t, "")
	for _, tc := range []struct {
		name, route, correlation, source, billing string
		height, status, streamCode, forwarded     int
	}{
		{"invalid dimensions", "/ai/generate-image", "", "proxy", "not_reserved", 4160, 400, 0, 0},
		{"upstream refusal", "/image/ai/generate-image", "http-error", "upstream", "refunded", 2304, 400, 0, 1},
		{"stream refusal", "/ai/generate-image-stream", "stream-error", "upstream", "refunded", 2304, 200, 402, 1},
		{"successful enhance", "/ai/generate-image-stream", "", "", "charged", 2304, 200, 0, 1},
		{"uncertain", "/ai/generate-image", "uncertain", "upstream", "pending", 2304, 502, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := longDimensionTestBody(1024, tc.height, "generate", true)
			body["input"] = logTestPrompt
			p := body["parameters"].(map[string]any)
			p["negative_prompt"] = "private-negative-prompt"
			p["v4_prompt"] = map[string]any{"caption": map[string]any{"base_caption": logTestPrompt, "char_captions": []any{map[string]string{"char_caption": "private-character-caption"}}}}
			before := calls.Load()
			w := logTestRequest(t, h, tc.route, logTestKey, body, tc.correlation)
			if w.Code != tc.status || int(calls.Load()-before) != tc.forwarded {
				t.Fatalf("request result changed: status=%d calls=%d", w.Code, calls.Load()-before)
			}
			list, err := h.logs.list(context.Background(), requestLogFilter{Query: w.Header().Get("X-Proxy-Request-Id"), Limit: 50})
			if err != nil || len(list.Items) != 1 {
				t.Fatalf("request was not recorded exactly once: items=%d err=%v", len(list.Items), err)
			}
			entry := list.Items[0]
			if entry.Width != 1024 || entry.Height != tc.height || entry.Steps != 28 || !entry.UpscaledEnhance || entry.Status != tc.status || entry.StreamErrorCode != tc.streamCode || entry.ErrorSource != tc.source || entry.BillingState != tc.billing {
				t.Fatalf("incorrect log metadata: %+v", entry)
			}
			if tc.forwarded > 0 && (entry.AccountID != defaultAccountID || entry.Provider != "novelai" || entry.UpstreamHost != "upstream.invalid") {
				t.Fatal("actual upstream account missing from log")
			}
			if tc.correlation == "http-error" || tc.correlation == "stream-error" {
				if !strings.Contains(entry.Error, "Required: 18") || !strings.Contains(entry.Error, "[redacted]") {
					t.Fatalf("useful error details lost: %q", entry.Error)
				}
			}
		})
	}
	files, _ := os.ReadDir(h.logs.dir)
	for _, file := range files {
		data, _ := os.ReadFile(filepath.Join(h.logs.dir, file.Name()))
		for _, secret := range []string{testNAIToken, testAdminKey, logTestKey, logTestPrompt, "private-negative-prompt", "private-character-caption", strings.Repeat("a", 100), "source-image"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("sensitive value leaked to request log: %q", secret)
			}
		}
	}
	failed, err := h.logs.list(context.Background(), requestLogFilter{ErrorsOnly: true, Limit: 50})
	if err != nil || len(failed.Items) != 4 {
		t.Fatalf("failure filter: count=%d err=%v", len(failed.Items), err)
	}
}

func TestRequestLogsRetentionPaginationAndRecovery(t *testing.T) {
	settings := &settingsStore{data: defaultProxySettings()}
	settings.data.LogMaxBytes = 1 << 20
	store := newRequestLogStore(filepath.Join(t.TempDir(), "keys.json"), settings)
	for i := 0; i < 450; i++ {
		entry := requestLog{RequestID: fmt.Sprintf("request-%d", i), CreatedAt: time.Now(), CompletedAt: time.Now(), Error: fmt.Sprintf("message-%d ", i) + strings.Repeat("safe ", 750), Outcome: "rejected", Status: 400, KeyID: "device"}
		if err := store.append(entry); err != nil {
			t.Fatal(err)
		}
		if stats := store.stats(); stats.Bytes > 1<<20 {
			t.Fatalf("log space limit exceeded: %d", stats.Bytes)
		}
	}
	list, err := store.list(context.Background(), requestLogFilter{Limit: 20})
	if err != nil || len(list.Items) != 20 || list.Items[0].RequestID != "request-449" || list.NextCursor == "" {
		t.Fatalf("latest logs or pagination incorrect: %+v err=%v", list, err)
	}
	older, err := store.list(context.Background(), requestLogFilter{Before: list.NextCursor, Limit: 20})
	if err != nil || len(older.Items) != 20 || older.Items[0].RequestID != "request-429" {
		t.Fatalf("cursor skipped or repeated records: count=%d err=%v", len(older.Items), err)
	}
	files, _ := os.ReadDir(store.dir)
	for _, file := range files {
		info, _ := file.Info()
		if info.Size() > logSegmentBytes || info.Mode().Perm() != 0600 {
			t.Fatalf("log segment size or permissions: %+v", info)
		}
	}
	last := filepath.Join(store.dir, files[len(files)-1].Name())
	f, _ := os.OpenFile(last, os.O_WRONLY|os.O_APPEND, 0600)
	_, _ = f.WriteString(`{"id":"partial-record`)
	_ = f.Close()
	reopened := newRequestLogStore(strings.TrimSuffix(store.dir, ".logs"), settings)
	if err := reopened.append(requestLog{RequestID: "after-restart", CreatedAt: time.Now(), CompletedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	list, err = reopened.list(context.Background(), requestLogFilter{Limit: 2})
	if err != nil || len(list.Items) != 2 || list.Items[0].RequestID != "after-restart" || list.Items[1].RequestID != "request-449" {
		t.Fatalf("partial append broke restart recovery: %+v err=%v", list, err)
	}
	old := time.Now().UTC().AddDate(0, 0, -8).Format("20060102-150405.000000000-") + "0123456789abcdef.jsonl"
	if err := os.WriteFile(filepath.Join(store.dir, old), []byte("expired\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reopened.prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, old)); !os.IsNotExist(err) {
		t.Fatal("expired log segment retained")
	}
	settings.mu.Lock()
	settings.data.LogDays = -1
	settings.mu.Unlock()
	_ = os.WriteFile(filepath.Join(store.dir, old), []byte("retained\n"), 0600)
	_ = reopened.prune()
	if _, err := os.Stat(filepath.Join(store.dir, old)); err != nil {
		t.Fatal("unlimited date retention deleted an old segment")
	}
}

func TestRequestLogsConcurrentWritesAndLoggingFailure(t *testing.T) {
	settings := &settingsStore{data: defaultProxySettings()}
	store := newRequestLogStore(filepath.Join(t.TempDir(), "keys.json"), settings)
	var group sync.WaitGroup
	for i := 0; i < 30; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if err := store.append(requestLog{RequestID: fmt.Sprint(i), CreatedAt: time.Now(), CompletedAt: time.Now()}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	group.Wait()
	list, err := store.list(context.Background(), requestLogFilter{Limit: 50})
	if err != nil || len(list.Items) != 30 {
		t.Fatalf("concurrent logs lost: count=%d err=%v", len(list.Items), err)
	}
	for i := 1; i < len(list.Items); i++ {
		if list.Items[i].ID >= list.Items[i-1].ID {
			t.Fatal("concurrent log cursor order is unstable")
		}
	}
	state := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(state+".logs", []byte("blocks log directory"), 0600); err != nil {
		t.Fatal(err)
	}
	h, _ := newLogTestHandler(t, state)
	w := logTestRequest(t, h, "/ai/generate-image", logTestKey, longDimensionTestBody(1024, 2304, "generate", false), "")
	key, _ := h.store.find(logTestKey)
	if w.Code != 200 || key.FixedSpent != 87 || key.SuccessfulGenerations != 1 || key.FixedPending != 0 || h.logs.stats().Failures == 0 {
		t.Fatal("logging failure affected generation or accounting")
	}
}

func TestRequestLogsAdminSettingsAndAuthorization(t *testing.T) {
	h, calls := newLogTestHandler(t, "")
	admin := func(method, path string, body any, key string) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, key := range []string{"", logTestKey} {
		if w := admin("GET", "/admin/logs", nil, key); w.Code != 401 {
			t.Fatal("request logs exposed without administrator authentication")
		}
	}
	for _, query := range []string{"?status=no", "?limit=0", "?before=../", "?from=bad", "?key_id=a&key_id=b", "?unknown=true", "?errors_only=yes"} {
		if w := admin("GET", "/admin/logs"+query, nil, testAdminKey); w.Code != 400 {
			t.Fatalf("invalid log filter accepted: %s status=%d", query, w.Code)
		}
	}
	for _, value := range []map[string]any{{"log_retention_days": 0}, {"log_retention_days": -2}, {"log_max_bytes": 1048575}, {"log_max_bytes": 1073741825}} {
		if w := admin("PUT", "/admin/settings", value, testAdminKey); w.Code != 400 {
			t.Fatal("invalid log retention accepted")
		}
	}
	if w := admin("PUT", "/admin/settings", map[string]any{"logs_enabled": false, "log_retention_days": -1, "log_max_bytes": 1 << 20}, testAdminKey); w.Code != 200 {
		t.Fatalf("log settings rejected: %s", w.Body.String())
	}
	_ = logTestRequest(t, h, "/ai/generate-image", logTestKey, longDimensionTestBody(1024, 2304, "generate", false), "")
	if h.logs.stats().Bytes != 0 || calls.Load() != 1 {
		t.Fatal("disabled logging still records or prevents generation")
	}
	_ = admin("PUT", "/admin/settings", map[string]bool{"logs_enabled": true}, testAdminKey)
	_ = logTestRequest(t, h, "/ai/generate-image", logTestKey, longDimensionTestBody(1024, 4160, "generate", false), "")
	w := admin("GET", "/admin/logs?key_id=log-device&status=400&errors_only=true", nil, testAdminKey)
	var list requestLogList
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Items) != 1 {
		t.Fatal("administrator cannot query filtered logs")
	}
	before, _ := h.store.find(logTestKey)
	if w := admin("DELETE", "/admin/logs?key_id=log-device", nil, testAdminKey); w.Code != 400 {
		t.Fatal("filtered deletion silently cleared all logs")
	}
	if w := admin("DELETE", "/admin/logs", nil, testAdminKey); w.Code != 204 || h.logs.stats().Bytes != 0 {
		t.Fatal("request log clear failed")
	}
	after, _ := h.store.find(logTestKey)
	if before.FixedSpent != after.FixedSpent || before.SuccessfulGenerations != after.SuccessfulGenerations {
		t.Fatal("clearing logs changed accounting")
	}
	reopenedSettings, err := decodeProxySettings([]byte(`{"allow_multi_image":true}`))
	if err != nil || !reopenedSettings.LogsEnabled || reopenedSettings.LogDays != 7 || reopenedSettings.LogMaxBytes != 100<<20 {
		t.Fatal("old settings did not receive logging defaults")
	}
	stored, _, _, savedSettings, _, err := openFixtureState(t, h.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.db.Close()
	if !savedSettings.data.LogsEnabled || savedSettings.data.LogDays != -1 || savedSettings.data.LogMaxBytes != 1<<20 {
		t.Fatal("log settings did not survive reopening the database")
	}
}

func TestRequestLogsQueueRefusalAndDurableExecution(t *testing.T) {
	h, calls := newLogTestHandler(t, "")
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		zero := 0
		keys[0].QueueLimit = &zero
		return keys, nil
	}); err != nil {
		t.Fatal(err)
	}
	release, err := h.enter(context.Background(), "blocker", "test", -1)
	if err != nil {
		t.Fatal(err)
	}
	w := logTestRequest(t, h, "/ai/generate-image", logTestKey, longDimensionTestBody(1024, 2304, "generate", false), "")
	release()
	if w.Code != 429 || calls.Load() != 0 {
		t.Fatal("queue refusal changed with logging enabled")
	}
	list, err := h.logs.list(context.Background(), requestLogFilter{Limit: 50})
	if err != nil || len(list.Items) != 1 || list.Items[0].ErrorSource != "proxy" || list.Items[0].BillingState != "not_reserved" {
		t.Fatal("queue refusal missing from request logs")
	}
	server := httptest.NewServer(h)
	defer server.Close()
	job := doManaged(t, managedRequest(t, "POST", server.URL+"/jobs/image/ai/generate-image-stream", logTestKey, longDimensionTestBody(1024, 2304, "generate", true)), 202)
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := doManaged(t, managedRequest(t, "GET", server.URL+"/jobs/"+job["id"].(string), logTestKey, nil), 200)
		if status["state"] == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable generation did not complete: %v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	list, err = h.logs.list(context.Background(), requestLogFilter{Query: job["id"].(string), Limit: 50})
	if err != nil || len(list.Items) != 1 || list.Items[0].JobID != job["id"] || list.Items[0].Outcome != "success" || !list.Items[0].Stream || calls.Load() != 1 {
		t.Fatalf("durable execution was logged incorrectly: %+v err=%v", list, err)
	}
}

func TestRequestLogsFinalImageBeforeDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	image := archiveTestPNG(color.RGBA{B: 255, A: 255})
	frame := archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": image})
	transport := settlementTransport(func(r *http.Request) (*http.Response, error) {
		var body io.ReadCloser
		if r.URL.Path == "/user/subscription" {
			body = io.NopCloser(strings.NewReader(`{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":10000,"purchasedTrainingSteps":0}}`))
		} else {
			body = &interruptedImageBody{data: bytes.NewReader(frame), cancel: cancel}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1}, nil
	})
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: "http://upstream.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginDrain)
	_ = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "close", Name: "closed", Hash: hexHash(logTestKey), AllowFixed: true, FixedLimit: 1000}), nil
	})
	data, _ := json.Marshal(longDimensionTestBody(1024, 2304, "generate", true))
	r := httptest.NewRequest("POST", "/ai/generate-image-stream", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+logTestKey)
	r = r.WithContext(context.WithValue(ctx, http.ServerContextKey, &http.Server{}))
	var aborted any
	func() {
		defer func() { aborted = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	list, err := h.logs.list(context.Background(), requestLogFilter{Limit: 50})
	key, _ := h.store.find(logTestKey)
	if aborted != http.ErrAbortHandler || err != nil || len(list.Items) != 1 || list.Items[0].Outcome != "success" || !list.Items[0].Interrupted || list.Items[0].BillingState != "charged" || key.SuccessfulGenerations != 1 || key.FixedPending != 0 {
		t.Fatalf("final frame/disconnect logging or accounting changed: abort=%v count=%d err=%v", aborted, len(list.Items), err)
	}
}
