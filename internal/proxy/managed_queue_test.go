package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPerKeyQueueLimitAndSnapshot(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: filepath.Join(t.TempDir(), "keys.json"), QueueSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	firstRelease, err := h.enter(context.Background(), "first", "POST /ai/generate-image", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queuedResult := make(chan error, 1)
	go func() {
		release, err := h.enter(ctx, "first", "POST /ai/upscale", 1)
		if err == nil {
			release()
		}
		queuedResult <- err
	}()
	waitForQueueLength(t, h, 1)
	if _, err := h.enter(context.Background(), "first", "POST /ai/upscale", 1); !errors.Is(err, errKeyQueueFull) {
		t.Fatalf("same key admission: %v", err)
	}
	if _, err := h.enter(context.Background(), "other", "POST /ai/upscale", 0); !errors.Is(err, errKeyQueueFull) {
		t.Fatalf("zero waiting slots: %v", err)
	}

	otherRelease := make(chan func(), 1)
	go func() {
		release, err := h.enter(context.Background(), "other", "POST /ai/encode-vibe", -1)
		if err == nil {
			otherRelease <- release
		}
	}()
	waitForQueueLength(t, h, 2)

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/queue", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized queue status = %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/queue", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminKey)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var state queueSnapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil {
		t.Fatalf("queue response: %d %s", response.Code, response.Body.String())
	}
	if state.Capacity != 2 || state.Active == nil || state.Active.KeyID != "first" || len(state.Waiting) != 2 || state.Waiting[0].KeyID != "first" || state.Waiting[1].KeyID != "other" || state.Waiting[1].Position != 2 {
		t.Fatalf("queue snapshot: %+v", state)
	}

	cancel()
	select {
	case err := <-queuedResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled wait did not return")
	}
	state = h.queueState()
	if len(state.Waiting) != 1 || state.Waiting[0].KeyID != "other" || state.Waiting[0].Position != 1 {
		t.Fatalf("canceled ticket remained in queue: %+v", state)
	}
	firstRelease()
	select {
	case release := <-otherRelease:
		release()
	case <-time.After(3 * time.Second):
		t.Fatal("next key did not run")
	}
	release, err := h.enter(context.Background(), "other", "POST /ai/upscale", 0)
	if err != nil {
		t.Fatalf("zero waiting slots must allow idle execution: %v", err)
	}
	release()
}

func waitForQueueLength(t *testing.T, h *ManagedHandler, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.queueLength() != count && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.queueLength(); got != count {
		t.Fatalf("queue length = %d, want %d", got, count)
	}
}

func TestQueueLimitPolicyPersistence(t *testing.T) {
	key := clientKey{ID: "old", Name: "old"}
	if viewKey(key).QueueLimit != -1 {
		t.Fatal("existing keys must default to unlimited waiting")
	}
	zero := 0
	if err := applyPolicy(&key, keyPolicyInput{QueueLimit: &zero}); err != nil || viewKey(key).QueueLimit != 0 {
		t.Fatalf("zero queue limit: %+v, %v", key, err)
	}
	invalid := 10001
	if err := applyPolicy(&key, keyPolicyInput{QueueLimit: &invalid}); err == nil {
		t.Fatal("expected invalid queue limit to fail")
	}
	key.QueueLimit = &zero
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(_ []clientKey) ([]clientKey, error) { return []clientKey{key}, nil }); err != nil {
		t.Fatal(err)
	}
	reopened, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyQueueLimit(reopened.snapshot()[0]); got != 0 {
		t.Fatalf("stored queue limit = %d", got)
	}
}

func TestQueueLimitAdminAPI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = w.Write([]byte(`{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "keys.json")
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: path, ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "bounded", "queue_limit": 2}), http.StatusCreated)
	client := created["client"].(map[string]any)
	if client["queue_limit"] != float64(2) {
		t.Fatalf("created queue limit: %v", client)
	}
	id := client["id"].(string)
	updated := doManaged(t, managedRequest(t, http.MethodPut, server.URL+"/admin/keys/"+id, testAdminKey, map[string]any{"queue_limit": 0}), http.StatusOK)
	if updated["queue_limit"] != float64(0) {
		t.Fatalf("updated queue limit: %v", updated)
	}
	clientQuota := doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/quota", created["key"].(string), nil), http.StatusOK)
	if clientQuota["queue_limit"] != float64(0) {
		t.Fatalf("client queue limit: %v", clientQuota)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := keyQueueLimit(restarted.store.snapshot()[0]); got != 0 {
		t.Fatalf("queue limit after restart = %d", got)
	}
}

func TestKeyQueueLimitRejectsConcurrentHTTPJob(t *testing.T) {
	started := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0}}`)
			return
		}
		close(started)
		<-resume
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL, QueueSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "no-wait", "allocation_anlas": 80, "queue_limit": 0}), http.StatusCreated)
	key := created["key"].(string)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	firstResult := make(chan int, 1)
	go func() {
		response, err := http.DefaultClient.Do(managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", key, job))
		if err != nil {
			firstResult <- -1
			return
		}
		defer response.Body.Close()
		firstResult <- response.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach upstream")
	}
	response, err := http.DefaultClient.Do(managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", key, job))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "key queue full") || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("second request: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
	close(resume)
	select {
	case status := <-firstResult:
		if status != http.StatusOK {
			t.Fatalf("first request status = %d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not finish")
	}
}
