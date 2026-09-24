package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDurableJobRestoresWaitingAndKeepsResult(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = io.WriteString(w, "image-data")
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "keys.json")
	config := ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: path, ImageUpstream: upstream.URL}
	first, err := NewManaged(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(first)
	defer server.Close()
	created := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "durable", "allow_fixed_anlas": true, "fixed_anlas_limit": 200}), http.StatusCreated)
	key := created["key"].(string)
	release, err := first.enter(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "blocker", "test", -1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	requestBody := `{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`
	newRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/jobs/ai/generate-image", strings.NewReader(requestBody))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "same-generation")
		return r
	}
	response := httptest.NewRecorder()
	first.ServeHTTP(response, newRequest())
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status %d: %s", response.Code, response.Body.String())
	}
	var submitted jobStatus
	if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil || !validJobID(submitted.ID) {
		t.Fatalf("submitted job: %+v, %v", submitted, err)
	}
	if first.queueLength() != 1 {
		t.Fatal("job was not queued")
	}
	duplicate := httptest.NewRecorder()
	first.ServeHTTP(duplicate, newRequest())
	var same jobStatus
	_ = json.Unmarshal(duplicate.Body.Bytes(), &same)
	if duplicate.Code != http.StatusAccepted || same.ID != submitted.ID || calls.Load() != 0 {
		t.Fatalf("idempotency failed: %d %+v", duplicate.Code, same)
	}
	first.BeginDrain()
	waitForQueueLength(t, first, 0)
	release()

	restarted, err := NewManaged(config)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var status jobStatus
	for time.Now().Before(deadline) {
		r := httptest.NewRequest(http.MethodGet, "/jobs/"+submitted.ID, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		check := httptest.NewRecorder()
		restarted.ServeHTTP(check, r)
		if check.Code != http.StatusOK {
			t.Fatalf("status %d: %s", check.Code, check.Body.String())
		}
		_ = json.Unmarshal(check.Body.Bytes(), &status)
		if status.State == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.State != "done" || status.Status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("recovered job: %+v, calls=%d", status, calls.Load())
	}
	resultRequest := httptest.NewRequest(http.MethodGet, status.ResultURL, nil)
	resultRequest.Header.Set("Authorization", "Bearer "+key)
	result := httptest.NewRecorder()
	restarted.ServeHTTP(result, resultRequest)
	if result.Code != http.StatusOK || result.Body.String() != "image-data" || result.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("saved result: %d %q %q", result.Code, result.Body.String(), result.Header().Get("Content-Type"))
	}
	if calls.Load() != 1 {
		t.Fatalf("job executed twice: %d", calls.Load())
	}
}

func TestRunningJobIsNotReplayed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	first, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	job := durableJob{ID: "j_" + strings.Repeat("a", 32), KeyID: "key", State: "running", Route: "/ai/generate-image", QueuedAt: time.Now(), StartedAt: time.Now()}
	if err := first.jobs.save(job); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := restarted.jobs.get(job.ID)
	if !ok || recovered.State != "interrupted" || recovered.FinishedAt.IsZero() || restarted.queueLength() != 0 {
		t.Fatalf("running job recovery: %+v, found=%v", recovered, ok)
	}
}

func TestTenConcurrentDurableSubmissions(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: filepath.Join(t.TempDir(), "keys.json"), QueueSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	const key = "pst-test-concurrent-submissions"
	ciphertext, err := h.vault.seal(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.update(func(_ []clientKey) ([]clientKey, error) {
		return []clientKey{{ID: "concurrent", Name: "concurrent", Hash: hexHash(key), KeyCiphertext: ciphertext}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	release, err := h.enter(context.Background(), "blocker", "test", -1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var group sync.WaitGroup
	statuses := make(chan int, 10)
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r := httptest.NewRequest(http.MethodPost, "/jobs/ai/generate-image", strings.NewReader(`{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`))
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			statuses <- w.Code
		}()
	}
	group.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Fatalf("concurrent submit returned %d", status)
		}
	}
	if got := h.queueLength(); got != 10 {
		t.Fatalf("queued %d of 10 jobs", got)
	}
	ids := make(chan string, 10)
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r := httptest.NewRequest(http.MethodPost, "/jobs/ai/generate-image", strings.NewReader(`{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`))
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "one-shared-request")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusAccepted {
				ids <- "failed"
				return
			}
			var status jobStatus
			_ = json.Unmarshal(w.Body.Bytes(), &status)
			ids <- status.ID
		}()
	}
	group.Wait()
	close(ids)
	var sharedID string
	for id := range ids {
		if !validJobID(id) || sharedID != "" && id != sharedID {
			t.Fatalf("concurrent idempotency returned %q, previous %q", id, sharedID)
		}
		sharedID = id
	}
	if got := h.queueLength(); got != 11 {
		t.Fatalf("duplicate submissions queued %d jobs, want 11", got)
	}
	h.BeginDrain()
	waitForQueueLength(t, h, 0)
}
