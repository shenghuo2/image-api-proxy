package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func archiveTestPNG(c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}

func archiveTestFrame(message map[string]any) []byte {
	data, _ := msgpack.Marshal(message)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	return frame
}

func waitArchiveCount(t *testing.T, h *ManagedHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := h.db.db.QueryRow("SELECT COUNT(*) FROM images").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("archive did not reach %d images", want)
}

func TestArchivePNGZIPStreamAndAdminAccess(t *testing.T) {
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	green := archiveTestPNG(color.RGBA{G: 255, A: 255})
	var zipData bytes.Buffer
	z := zip.NewWriter(&zipData)
	for name, data := range map[string][]byte{"one.png": red, "two.png": green} {
		f, _ := z.Create(name)
		_, _ = f.Write(data)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	stream := append(archiveTestFrame(map[string]any{"code": 200, "step_ix": 1, "image": red}), archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": green})...)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		var data []byte
		if strings.HasSuffix(r.URL.Path, "stream") {
			data = stream
		} else if r.Header.Get("X-Correlation-Id") == "zip" {
			data = zipData.Bytes()
		} else {
			data = red
		}
		w.WriteHeader(200)
		_, _ = w.Write(data)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/settings", testAdminKey, map[string]any{"archive_enabled": true, "allow_multi_image": true}), 200)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "archive-key", "allow_fixed_anlas": true, "fixed_anlas_limit": 500, "allow_multi_image": true}), 201)
	key := created["key"].(string)
	requestBody := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	call := func(path, marker string, expected []byte) {
		t.Helper()
		req := managedRequest(t, "POST", server.URL+path, key, requestBody)
		req.Header.Set("X-Forwarded-For", "203.0.113.99")
		if marker != "" {
			req.Header.Set("X-Correlation-Id", marker)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !bytes.Equal(body, expected) {
			t.Fatalf("response changed: status=%d bytes=%d", resp.StatusCode, len(body))
		}
	}
	call("/ai/generate-image", "", red)
	waitArchiveCount(t, h, 1)
	call("/image/ai/generate-image", "zip", zipData.Bytes())
	waitArchiveCount(t, h, 3)
	call("/ai/generate-image-stream", "", stream)
	waitArchiveCount(t, h, 4)
	var finalName string
	if err := h.db.db.QueryRow("SELECT original FROM images ORDER BY rowid DESC LIMIT 1").Scan(&finalName); err != nil {
		t.Fatal(err)
	}
	finalData, err := os.ReadFile(h.archive.path(finalName, ""))
	if err != nil || !bytes.Equal(finalData, green) {
		t.Fatal("stream preview was saved instead of final frame")
	}
	var groupCount int
	if err := h.db.db.QueryRow("SELECT COUNT(*) FROM (SELECT group_id FROM images GROUP BY group_id HAVING COUNT(*)=2)").Scan(&groupCount); err != nil || groupCount != 1 {
		t.Fatalf("zip group count %d: %v", groupCount, err)
	}
	var id, thumbnail string
	if err := h.db.db.QueryRow("SELECT id,thumbnail FROM images ORDER BY created_at DESC,id DESC LIMIT 1").Scan(&id, &thumbnail); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.archive.path(thumbnail, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 100<<10 {
		t.Fatalf("thumbnail too large: %d", len(data))
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/admin/images/"+id+"/thumbnail", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("image leaked without admin key: %d", unauthorized.Code)
	}
	adminReq := httptest.NewRequest("GET", "/admin/images/"+id+"/thumbnail", nil)
	adminReq.Header.Set("Authorization", "Bearer "+testAdminKey)
	adminResponse := httptest.NewRecorder()
	h.ServeHTTP(adminResponse, adminReq)
	if adminResponse.Code != 200 || adminResponse.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(adminResponse.Body.Bytes(), data) {
		t.Fatal("admin thumbnail response")
	}
	list := httptest.NewRequest("GET", "/admin/images?key_id="+created["client"].(map[string]any)["id"].(string)+"&page=1", nil)
	list.Header.Set("Authorization", "Bearer "+testAdminKey)
	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, list)
	if listed.Code != 200 {
		t.Fatalf("list status %d: %s", listed.Code, listed.Body.String())
	}
	var result struct {
		Items []archiveImage `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &result); err != nil || result.Total != 4 {
		t.Fatalf("list: %+v: %v", result, err)
	}
	multiItems := 0
	for _, item := range result.Items {
		if item.IP == "203.0.113.99" || item.KeyName != "archive-key" {
			t.Fatalf("spoofed IP or missing key name: %+v", item)
		}
		if item.GroupSize == 2 {
			multiItems++
		}
	}
	if multiItems != 2 {
		t.Fatalf("ZIP grouping missing: %d", multiItems)
	}
	originalReq := httptest.NewRequest("GET", "/admin/images/"+id+"/original", nil)
	originalReq.Header.Set("Authorization", "Bearer "+testAdminKey)
	originalResp := httptest.NewRecorder()
	h.ServeHTTP(originalResp, originalReq)
	if originalResp.Code != 200 || originalResp.Header().Get("Content-Type") != "image/png" {
		t.Fatal("original download failed")
	}
	deleteReq := httptest.NewRequest("DELETE", "/admin/images/"+id, nil)
	deleteReq.Header.Set("Authorization", "Bearer "+testAdminKey)
	deleted := httptest.NewRecorder()
	h.ServeHTTP(deleted, deleteReq)
	if deleted.Code != 204 {
		t.Fatalf("delete status %d", deleted.Code)
	}
	waitArchiveCount(t, h, 3)
	for _, item := range result.Items {
		if item.ID == id {
			continue
		}
		req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+item.ID, nil)
		req.Header.Set("Authorization", "Bearer "+testAdminKey)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != http.StatusNoContent {
			t.Fatalf("delete remaining image: %d", response.Code)
		}
	}
	stats := archiveAdminGet(t, h, "/admin/images/stats")
	var summary struct {
		Count        int  `json:"count"`
		EverArchived bool `json:"ever_archived"`
	}
	if stats.Code != http.StatusOK || json.Unmarshal(stats.Body.Bytes(), &summary) != nil || summary.Count != 0 || !summary.EverArchived {
		t.Fatalf("archive history lost after deleting images: %d %s", stats.Code, stats.Body.String())
	}
}

func TestArchiveStreamingFlushesBeforeCompletion(t *testing.T) {
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	preview := archiveTestFrame(map[string]any{"code": 200, "step_ix": 1, "image": red})
	final := archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": red})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		_, _ = w.Write(preview)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write(final)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/settings", testAdminKey, map[string]any{"archive_enabled": true}), 200)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "streaming", "allow_fixed_anlas": true, "fixed_anlas_limit": 100}), 201)
	body := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image-stream", created["key"].(string), body).WithContext(ctx))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	received := make(chan error, 1)
	go func() {
		first := make([]byte, len(preview))
		_, err := io.ReadFull(resp.Body, first)
		if err == nil && !bytes.Equal(first, preview) {
			err = errors.New("preview frame changed")
		}
		received <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("stream buffered until final frame")
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, final) {
		t.Fatal("final frame changed")
	}
	waitArchiveCount(t, h, 1)
}

func TestArchiveStreamErrorsAndCaptureFailure(t *testing.T) {
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	for _, data := range [][]byte{
		archiveTestFrame(map[string]any{"code": 500, "step_ix": nil, "image": red}),
		archiveTestFrame(map[string]any{"code": 200, "step_ix": 1, "image": red}),
		append(archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": red}), 0),
	} {
		if _, err := extractStream(bytes.NewReader(data)); err == nil {
			t.Fatal("invalid stream archived")
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		_, _ = w.Write(red)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/settings", testAdminKey, map[string]any{"archive_enabled": true}), 200)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "fail-key", "allow_fixed_anlas": true, "fixed_anlas_limit": 100, "archive_enabled": false}), 201)
	key := created["key"].(string)
	requestBody := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, requestBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitArchiveCount(t, h, 0)
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/keys/"+created["client"].(map[string]any)["id"].(string), testAdminKey, map[string]any{"archive_enabled": true}), 200)
	h.archive.dir = filepath.Join(t.TempDir(), "missing")
	resp, err = http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, requestBody))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, red) {
		t.Fatal("archive failure changed generation response")
	}
	var failures int
	if err := h.db.db.QueryRow("SELECT value FROM meta WHERE name='archive_failures'").Scan(&failures); err != nil || failures < 1 {
		t.Fatalf("failure counter %d: %v", failures, err)
	}
}

func TestArchiveIPTrustAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	raw := "pst-OldKey1234567890AbCdEfgh"
	key := clientKey{ID: "legacy", Name: "before", Hash: hexHash(raw), PolicyVersion: 1, AllowFixed: true, FixedLimit: 20, FixedSpent: 7}
	data, _ := json.Marshal([]clientKey{key})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	vault := newKeyVault(testAdminKey)
	ciphertext, err := vault.seal(testNAIToken)
	if err != nil {
		t.Fatal(err)
	}
	accountData, _ := json.Marshal([]upstreamAccount{{ID: "default", Name: "old-account", TokenCiphertext: ciphertext}})
	if err := os.WriteFile(path+".accounts.json", accountData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".settings.json", []byte(`{"allow_multi_image":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	jobID := "j_" + strings.Repeat("a", 32)
	if err := os.MkdirAll(path+".jobs", 0700); err != nil {
		t.Fatal(err)
	}
	job := durableJob{ID: jobID, KeyID: "legacy", Route: "/ai/generate-image", State: "done", QueuedAt: time.Now(), FinishedAt: time.Now(), Status: 200}
	jobData, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(path+".jobs", jobID+".json"), jobData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".jobs", jobID+".result"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path, TrustedProxyCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	settings := h.settings.snapshot()
	if got, ok := h.store.find(raw); !ok || got.FixedSpent != 7 || !settings.AllowMultiImage || settings.ArchiveDays != 30 || settings.ArchiveMaxBytes != 20<<30 || len(h.accounts.snapshot()) != 1 {
		t.Fatal("legacy state lost")
	}
	var schemaVersion int
	if err := h.db.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion != stateSchemaVersion {
		t.Fatalf("legacy import schema version=%d err=%v", schemaVersion, err)
	}
	if _, ok := h.jobs.get(jobID); !ok {
		t.Fatal("legacy job lost")
	}
	backup, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(backup, data) {
		t.Fatal("legacy backup changed")
	}
	if err := os.WriteFile(path, []byte("broken backup"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.store.find(raw); !ok {
		t.Fatal("database restart read old JSON")
	}
	r := httptest.NewRequest("POST", "/ai/generate-image", nil)
	r.RemoteAddr = "192.0.2.4:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.4")
	if h.clientIP(r) != "192.0.2.4" {
		t.Fatal("direct spoofed forwarded header")
	}
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.4, 10.4.5.6")
	if h.clientIP(r) != "203.0.113.4" {
		t.Fatal("trusted proxy chain not parsed")
	}
	local, err := parseTrustedProxies(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	h.trustedProxies = local
	r.RemoteAddr = "127.0.0.1:4567"
	r.Header.Set("X-Forwarded-For", "198.51.100.27")
	if h.clientIP(r) != "198.51.100.27" {
		t.Fatal("loopback proxy IP not resolved")
	}
	if gateway := dockerGateway(); gateway != nil {
		r.RemoteAddr = gateway.String() + ":4567"
		if h.clientIP(r) != "198.51.100.27" {
			t.Fatal("local Docker gateway not trusted")
		}
	}
}

func TestArchiveRestartRecoveryAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	keys := &keyStore{path: path, keys: []clientKey{}}
	accounts := &accountStore{path: path + ".accounts.json", accounts: []upstreamAccount{}}
	settingsStore := &settingsStore{path: path + ".settings.json", data: defaultProxySettings()}
	jobs := &jobStore{dir: path + ".jobs", jobs: make(map[string]durableJob)}
	initial, err := openStateDB(path, keys, accounts, settingsStore, jobs, newKeyVault(testAdminKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.db.Close(); err != nil {
		t.Fatal(err)
	}
	archiveDir := path + ".archive"
	if err := os.MkdirAll(archiveDir, 0700); err != nil {
		t.Fatal(err)
	}
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	id := strings.Repeat("b", 32)
	work := archiveWork{ID: id, GroupID: id, KeyID: "key", KeyName: "at-submit", IP: "192.0.2.1", Route: "/ai/generate-image", Completed: time.Now()}
	if err := os.WriteFile(filepath.Join(archiveDir, id+".capture"), red, 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(work)
	stagedDB, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stagedDB.Exec("INSERT INTO archive_work(id,data,created_at) VALUES(?,?,?)", id, data, time.Now().Unix()); err != nil {
		_ = stagedDB.Close()
		t.Fatal(err)
	}
	if err := stagedDB.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	waitArchiveCount(t, restarted, 1)
	settings := restarted.settings.snapshot()
	settings.ArchiveDays = -1
	settings.ArchiveMaxBytes = 1 << 20
	if err := restarted.settings.set(settings); err != nil {
		t.Fatal(err)
	}
	if err := restarted.archive.prune(settings); err != nil {
		t.Fatal(err)
	}
	waitArchiveCount(t, restarted, 1)
	settings.ArchiveMaxBytes = 1
	if err := restarted.archive.prune(settings); err != nil {
		t.Fatal(err)
	}
	waitArchiveCount(t, restarted, 0)
}

func TestArchiveMigrationRejectsMissingJobResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.MkdirAll(path+".jobs", 0700); err != nil {
		t.Fatal(err)
	}
	jobID := "j_" + strings.Repeat("c", 32)
	data, _ := json.Marshal(durableJob{ID: jobID, KeyID: "key", State: "done"})
	if err := os.WriteFile(filepath.Join(path+".jobs", jobID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path}); err == nil {
		t.Fatal("migration accepted missing result")
	}
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables, version int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('meta','keys','settings')").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("partial schema after failed migration: tables=%d, err=%v", tables, err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("schema version after failed migration: version=%d, err=%v", version, err)
	}
}

func TestArchiveMigratesWaitingLegacyJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	raw := "pst-LegacyWaitingKeyAbc1234567"
	vault := newKeyVault(testAdminKey)
	keyCipher, err := vault.seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	accountCipher, err := vault.seal(testNAIToken)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := json.Marshal([]clientKey{{ID: "legacy", Name: "waiting", Hash: hexHash(raw), KeyCiphertext: keyCipher, PolicyVersion: 1, AllowFixed: true, FixedLimit: 100}})
	if err := os.WriteFile(path, keys, 0600); err != nil {
		t.Fatal(err)
	}
	accounts, _ := json.Marshal([]upstreamAccount{{ID: defaultAccountID, Name: "upstream", TokenCiphertext: accountCipher}})
	if err := os.WriteFile(path+".accounts.json", accounts, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path+".jobs", 0700); err != nil {
		t.Fatal(err)
	}
	id := "j_" + strings.Repeat("d", 32)
	body := []byte(`{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`)
	bodyHash := sha256.Sum256(body)
	job := durableJob{ID: id, KeyID: "legacy", KeyHash: hexHash(raw), BodyHash: hex.EncodeToString(bodyHash[:]), Route: "/ai/generate-image", ContentType: "application/json", State: "waiting", QueuedAt: time.Now(), ClientIP: "198.51.100.9"}
	metadata, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(path+".jobs", id+".json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".jobs", id+".body"), body, 0600); err != nil {
		t.Fatal(err)
	}
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		_, _ = w.Write(red)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path, ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := h.jobs.get(id)
		if ok && current.State == "done" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	current, ok := h.jobs.get(id)
	if !ok || current.State != "done" || current.ClientIP != "198.51.100.9" {
		t.Fatalf("migrated waiting job: %+v", current)
	}
	result, err := os.ReadFile(h.jobs.path(id, ".result"))
	if err != nil || !bytes.Equal(result, red) {
		t.Fatal("migrated job result missing")
	}
	backup, err := os.ReadFile(h.jobs.path(id, ".json"))
	if err != nil || !bytes.Equal(backup, metadata) {
		t.Fatal("legacy job metadata backup changed")
	}
}

func TestArchiveClientDisconnectDoesNotCommit(t *testing.T) {
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		_, _ = w.Write(red[:len(red)/2])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/settings", testAdminKey, map[string]any{"archive_enabled": true}), 200)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "disconnect", "allow_fixed_anlas": true, "fixed_anlas_limit": 100}), 201)
	body := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := managedRequest(t, "POST", server.URL+"/ai/generate-image", created["key"].(string), body).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 8)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	var count int
	if err := h.db.db.QueryRow("SELECT COUNT(*) FROM archive_work").Scan(&count); err != nil || count != 0 {
		t.Fatalf("disconnected response queued for archive: %d, %v", count, err)
	}
}

func TestArchiveDurableJobUsesSubmissionIP(t *testing.T) {
	red := archiveTestPNG(color.RGBA{R: 255, A: 255})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		_, _ = w.Write(red)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/settings", testAdminKey, map[string]any{"archive_enabled": true}), 200)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "durable-archive", "allow_fixed_anlas": true, "fixed_anlas_limit": 100}), 201)
	body := `{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`
	req := httptest.NewRequest("POST", "/jobs/ai/generate-image", strings.NewReader(body))
	req.RemoteAddr = "198.51.100.18:4321"
	req.Header.Set("Authorization", "Bearer "+created["key"].(string))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 202 {
		t.Fatalf("submit status %d: %s", response.Code, response.Body.String())
	}
	var submitted jobStatus
	if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	waitArchiveCount(t, h, 1)
	var groupID, ip, keyName string
	if err := h.db.db.QueryRow("SELECT group_id,ip,key_name FROM images LIMIT 1").Scan(&groupID, &ip, &keyName); err != nil {
		t.Fatal(err)
	}
	if groupID != submitted.ID || ip != "198.51.100.18" || keyName != "durable-archive" {
		t.Fatalf("durable metadata: group=%s ip=%s key=%s", groupID, ip, keyName)
	}
}
