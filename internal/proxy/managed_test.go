package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const testAdminKey = "admin-0123456789abcdef0123456789abcdef"
const testNAIToken = "nai-server-token-0123456789"

func managedRequest(t *testing.T, method, url, key string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func doManaged(t *testing.T, req *http.Request, status int) map[string]any {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status %d, want %d: %s", req.Method, req.URL, resp.StatusCode, status, data)
	}
	if status == http.StatusNoContent {
		return nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestManagedKeysQuotaAndPersistence(t *testing.T) {
	var mu sync.Mutex
	balance := int64(100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testNAIToken {
			t.Error("upstream received wrong credentials")
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/user/subscription" {
			fmt.Fprintf(w, `{"active":false,"isGracePeriod":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":%d,"purchasedTrainingSteps":0},"usage":{"percent":90}}`, balance)
			return
		}
		if r.URL.Path == "/ai/generate-image" {
			balance -= 7
			w.Write([]byte("image"))
			return
		}
		t.Errorf("unexpected upstream path: %s", r.URL.Path)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "keys.json")
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: path, ImageUpstream: upstream.URL, QueueSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "alice", "allocation_anlas": 40}), 201)
	clientKey := created["key"].(string)
	id := created["client"].(map[string]any)["id"].(string)
	if len(clientKey) != 64 {
		t.Fatal("invalid client key")
	}
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "too much", "allocation_anlas": 61}), 409)
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), 200)
	if quota["unallocated_anlas"] != float64(60) {
		t.Fatalf("unallocated = %v", quota)
	}
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", clientKey, job))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != "image" {
		t.Fatalf("job: %d %s", resp.StatusCode, data)
	}
	clientQuota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", clientKey, nil), 200)
	if clientQuota["spent_anlas"] != float64(9) || clientQuota["remaining_anlas"] != float64(31) || clientQuota["pending_anlas"] != float64(0) {
		t.Fatalf("client quota = %v", clientQuota)
	}
	subscription := doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", clientKey, nil), 200)
	steps := subscription["trainingStepsLeft"].(map[string]any)
	if steps["fixedTrainingStepsLeft"] != float64(31) {
		t.Fatalf("subscription = %v", subscription)
	}
	if subscription["active"] != false || subscription["isGracePeriod"] != true || subscription["tier"] != float64(2) {
		t.Fatalf("subscription status = %v", subscription)
	}
	reopened, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted, ok := reopened.find(clientKey); !ok || persisted.FixedSpent != 9 {
		t.Fatalf("persisted key = %+v, found = %v", persisted, ok)
	}
	doManaged(t, managedRequest(t, "DELETE", server.URL+"/admin/keys/"+id, testAdminKey, nil), 204)
	resp, err = http.DefaultClient.Do(managedRequest(t, "GET", server.URL+"/quota", clientKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("revoked key status = %d", resp.StatusCode)
	}
}

func TestManagedQueueSerializesJobs(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	active, maximum, jobs := 0, 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if r.URL.Path == "/user/subscription" {
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0}}`)
			return
		}
		mu.Lock()
		jobs++
		n := jobs
		mu.Unlock()
		if n == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL, QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "alice", "allocation_anlas": 80}), 201)
	clientKey := created["key"].(string)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	results := make(chan int, 2)
	send := func() {
		resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", clientKey, job))
		if err != nil {
			results <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		results <- resp.StatusCode
	}
	go send()
	select {
	case <-firstEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first job did not reach upstream")
	}
	go send()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.queue) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.queue) != 1 {
		t.Fatal("second job was not queued")
	}
	third := managedRequest(t, "POST", server.URL+"/ai/generate-image", clientKey, job)
	response, err := http.DefaultClient.Do(third)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 429 {
		t.Fatalf("full queue status = %d", response.StatusCode)
	}
	close(releaseFirst)
	for i := 0; i < 2; i++ {
		select {
		case status := <-results:
			if status != 200 {
				t.Fatalf("job status = %d", status)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued job did not finish")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maximum != 1 || jobs != 2 {
		t.Fatalf("maximum concurrent = %d, jobs = %d", maximum, jobs)
	}
}

func TestManagedInsufficientQuotaAndReconciliation(t *testing.T) {
	var jobs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0}}`)
			return
		}
		jobs.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, "unavailable")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	low := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "low", "allocation_anlas": 5}), 201)
	good := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "good", "allocation_anlas": 20}), 201)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	badResponse, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", low["key"].(string), job))
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusPaymentRequired || jobs.Load() != 0 {
		t.Fatalf("insufficient quota: status %d, upstream jobs %d", badResponse.StatusCode, jobs.Load())
	}
	goodKey := good["key"].(string)
	goodResponse, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", goodKey, job))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, goodResponse.Body)
	goodResponse.Body.Close()
	if goodResponse.StatusCode != 503 || jobs.Load() != 1 {
		t.Fatalf("accepted job: status %d, upstream jobs %d", goodResponse.StatusCode, jobs.Load())
	}
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", goodKey, nil), 200)
	pending := quota["pending_anlas"].(float64)
	if pending <= 0 || quota["spent_anlas"] != pending {
		t.Fatalf("pending quota = %v", quota)
	}
	id := good["client"].(map[string]any)["id"].(string)
	reconciled := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys/"+id+"/reconcile", testAdminKey, map[string]any{"charged_anlas": 7}), 200)
	if reconciled["spent_anlas"] != float64(7) || reconciled["pending_anlas"] != float64(0) {
		t.Fatalf("reconciled quota = %v", reconciled)
	}
}

func TestReconciliationAfterQuotaRefreshDoesNotDoubleCount(t *testing.T) {
	var balance atomic.Int64
	balance.Store(100)
	var quotaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			quotaCalls.Add(1)
			fmt.Fprintf(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":%d,"purchasedTrainingSteps":0}}`, balance.Load())
			return
		}
		balance.Add(-7)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL, QuotaTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "pending", "allocation_anlas": 20}), http.StatusCreated)
	key := created["key"].(string)
	id := created["client"].(map[string]any)["id"].(string)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job), http.StatusServiceUnavailable)
	time.Sleep(1100 * time.Millisecond)
	before := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), http.StatusOK)
	if before["projected_fixed_anlas"] != float64(93) {
		t.Fatalf("refreshed quota = %v", before)
	}
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys/"+id+"/reconcile", testAdminKey, map[string]any{"charged_anlas": 7}), http.StatusOK)
	after := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), http.StatusOK)
	if after["projected_fixed_anlas"] != float64(93) || quotaCalls.Load() != 3 {
		t.Fatalf("quota after reconciliation = %v, calls = %d", after, quotaCalls.Load())
	}
}

func TestOfficialBearerAndRouteCompatibility(t *testing.T) {
	image := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testNAIToken {
			t.Error("upstream did not receive the server token")
		}
		switch r.URL.Path {
		case "/user/subscription":
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
		case "/ai/generate-image":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Set-Cookie", "upstream=secret")
			w.Write([]byte{0, 1, 2, 255})
		case "/ai/upscale":
			io.WriteString(w, "image-upscale")
		default:
			t.Errorf("unexpected image path %s", r.URL.Path)
		}
	}))
	defer image.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: image.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "client", "allocation_anlas": 800}), 201)
	clientKey := created["key"].(string)

	for _, tc := range []struct {
		path, want string
	}{
		{"/ai/upscale", "image-upscale"},
		{"/image/ai/upscale", "image-upscale"},
	} {
		resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+tc.path, clientKey, map[string]any{"image": "data"}))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(data) != tc.want {
			t.Fatalf("%s: status %d, body %s", tc.path, resp.StatusCode, data)
		}
	}
	doManaged(t, managedRequest(t, "POST", server.URL+"/api/ai/upscale", clientKey, map[string]any{"image": "data"}), http.StatusNotFound)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	resp, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", clientKey, job))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(data, []byte{0, 1, 2, 255}) || resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("official response: status %d, headers %v, body %v", resp.StatusCode, resp.Header, data)
	}
	for _, authorization := range []string{"", "Basic " + clientKey, "Bearer wrong", "Bearer " + testAdminKey} {
		req := managedRequest(t, "GET", server.URL+"/quota", clientKey, nil)
		if authorization == "" {
			req.Header.Del("Authorization")
		} else {
			req.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatalf("authorization %q accepted: %d", authorization, response.StatusCode)
		}
	}
	duplicate := managedRequest(t, "GET", server.URL+"/quota", clientKey, nil)
	duplicate.Header.Add("Authorization", "Bearer "+clientKey)
	response, err := http.DefaultClient.Do(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("duplicate authorization status = %d", response.StatusCode)
	}
	doManaged(t, managedRequest(t, "GET", server.URL+"/admin/keys", clientKey, nil), 401)
	doManaged(t, managedRequest(t, "POST", server.URL+"/user/subscription", clientKey, nil), 405)
	doManaged(t, managedRequest(t, "POST", server.URL+"/user/login", clientKey, nil), 404)
}

func TestManagedStreamCancellation(t *testing.T) {
	firstFrame := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0}}`)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 0, 0, 1, 0x91})
		w.(http.Flusher).Flush()
		close(firstFrame)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "stream", "allocation_anlas": 50}), 201)
	key := created["key"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := managedRequest(t, "POST", server.URL+"/ai/generate-image-stream", key, map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-firstFrame
	frame := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, frame); err != nil || !bytes.Equal(frame, []byte{0, 0, 0, 1, 0x91}) {
		t.Fatalf("streamed frame = %v, error = %v", frame, err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
	doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", key, nil), 200)
}

func TestSeparateAnlasPermissions(t *testing.T) {
	var quotaCalls atomic.Int32
	var jobs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			quotaCalls.Add(1)
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":20,"purchasedTrainingSteps":20}}`)
			return
		}
		jobs.Add(1)
		io.WriteString(w, "image")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	issue := func(name string, policy map[string]any) (string, string) {
		policy["name"] = name
		created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, policy), 201)
		return created["key"].(string), created["client"].(map[string]any)["id"].(string)
	}
	fixedKey, _ := issue("fixed", map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 15})
	purchasedKey, purchasedID := issue("purchased", map[string]any{"allow_purchased_anlas": true, "purchased_anlas_limit": 15})
	blockedKey, _ := issue("blocked", map[string]any{"fixed_anlas_limit": 5, "allow_fixed_anlas": false})
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	for _, key := range []string{fixedKey, purchasedKey} {
		response, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("allowed key status = %d", response.StatusCode)
		}
	}
	blocked, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", blockedKey, job))
	if err != nil {
		t.Fatal(err)
	}
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusPaymentRequired || jobs.Load() != 2 {
		t.Fatalf("disabled key status = %d, jobs = %d", blocked.StatusCode, jobs.Load())
	}
	fixed := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", fixedKey, nil), 200)
	purchased := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", purchasedKey, nil), 200)
	if fixed["fixed_anlas_spent"] != float64(9) || fixed["purchased_anlas_spent"] != float64(0) || purchased["fixed_anlas_spent"] != float64(0) || purchased["purchased_anlas_spent"] != float64(9) {
		t.Fatalf("split billing: fixed=%v purchased=%v", fixed, purchased)
	}
	subscription := doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", purchasedKey, nil), 200)
	steps := subscription["trainingStepsLeft"].(map[string]any)
	if steps["fixedTrainingStepsLeft"] != float64(0) || steps["purchasedTrainingSteps"] != float64(6) {
		t.Fatalf("client subscription = %v", subscription)
	}
	if quotaCalls.Load() != 1 {
		t.Fatalf("quota queried %d times within TTL", quotaCalls.Load())
	}
	updated := doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/keys/"+purchasedID, testAdminKey, map[string]any{"allow_purchased_anlas": false}), 200)
	if updated["purchased_anlas_remaining"] != float64(0) {
		t.Fatalf("disabled purchased quota = %v", updated)
	}
}

func TestOpusLimitAndQuotaRefresh(t *testing.T) {
	var quotaCalls atomic.Int32
	var jobs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			quotaCalls.Add(1)
			io.WriteString(w, `{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":50,"purchasedTrainingSteps":0},"usage":{"percent":90,"isNegative":false}}`)
			return
		}
		jobs.Add(1)
		io.WriteString(w, "image")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL, QuotaTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	opus := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "opus", "allow_opus": true, "opus_limit_images": 1}), 201)
	paid := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "paid", "allow_fixed_anlas": true, "fixed_anlas_limit": 10}), 201)
	opusKey, paidKey := opus["key"].(string), paid["key"].(string)
	job := map[string]any{"model": "nai-diffusion-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	response, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", opusKey, job))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("Opus job status = %d", response.StatusCode)
	}
	for _, key := range []string{opusKey, paidKey} {
		response, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("Opus-disabled or exhausted key status = %d", response.StatusCode)
		}
	}
	if jobs.Load() != 1 || quotaCalls.Load() != 1 {
		t.Fatalf("jobs=%d quota calls=%d", jobs.Load(), quotaCalls.Load())
	}
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", opusKey, nil), 200)
	if quota["opus_used_images"] != float64(1) || quota["opus_remaining_images"] != float64(0) || quota["spent_anlas"] != float64(0) {
		t.Fatalf("Opus quota = %v", quota)
	}
	subscription := doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", opusKey, nil), 200)
	if subscription["usage"] != nil {
		t.Fatalf("exhausted key exposes shared Opus battery: %v", subscription)
	}
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/quota/refresh", testAdminKey, nil), 200)
	if quotaCalls.Load() != 1 {
		t.Fatalf("manual refresh ignored minimum interval: %d", quotaCalls.Load())
	}
	time.Sleep(1100 * time.Millisecond)
	doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), 200)
	if quotaCalls.Load() != 2 {
		t.Fatalf("quota refresh count = %d, want 2", quotaCalls.Load())
	}
}

func TestLegacyKeyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	data := fmt.Sprintf(`[{"id":"old","name":"existing","hash":%q,"allocated":30,"spent":7,"pending":2}]`, hexHash("old-secret"))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := store.find("old-secret")
	if !ok || !key.AllowFixed || key.FixedLimit != 30 || key.FixedSpent != 7 || key.FixedPending != 2 || key.AllowPurchased || key.AllowOpus {
		t.Fatalf("migrated key = %+v, found = %v", key, ok)
	}
	if err := store.update(func(keys []clientKey) ([]clientKey, error) { return keys, nil }); err != nil {
		t.Fatal(err)
	}
	reopened, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key, ok = reopened.find("old-secret")
	if !ok || key.FixedLimit != 30 || key.FixedSpent != 7 || key.FixedPending != 2 || key.PolicyVersion != 1 {
		t.Fatalf("persisted migration = %+v, found = %v", key, ok)
	}
}

func TestQuotaRefreshFailureBackoff(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	for i := 0; i < 3; i++ {
		doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), http.StatusBadGateway)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream quota calls = %d, want 1", calls.Load())
	}
}

func TestUpstreamRejectionRefundsReservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":20,"purchasedTrainingSteps":0}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "reject", "allow_fixed_anlas": true, "fixed_anlas_limit": 10}), 201)
	key := created["key"].(string)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	response, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("upstream status = %d", response.StatusCode)
	}
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", key, nil), 200)
	if quota["fixed_anlas_spent"] != float64(0) || quota["fixed_anlas_pending"] != float64(0) || quota["fixed_anlas_remaining"] != float64(10) {
		t.Fatalf("reservation was not refunded: %v", quota)
	}
}

func TestLowOpusBatteryFailsClosedAndEmptyBatteryUsesAnlas(t *testing.T) {
	var percent atomic.Int64
	percent.Store(4)
	var jobs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			p := percent.Load()
			fmt.Fprintf(w, `{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":20,"purchasedTrainingSteps":0},"usage":{"percent":%d,"isNegative":%t}}`, p, p == 0)
			return
		}
		jobs.Add(1)
		io.WriteString(w, "image")
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL, QuotaTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "paid", "allow_fixed_anlas": true, "fixed_anlas_limit": 15}), 201)
	key := created["key"].(string)
	job := map[string]any{"model": "nai-diffusion-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	first, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusPaymentRequired || jobs.Load() != 0 {
		t.Fatalf("low battery status=%d jobs=%d", first.StatusCode, jobs.Load())
	}
	percent.Store(0)
	time.Sleep(1100 * time.Millisecond)
	second, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != 200 || jobs.Load() != 1 {
		t.Fatalf("empty battery status=%d jobs=%d", second.StatusCode, jobs.Load())
	}
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", key, nil), 200)
	if quota["fixed_anlas_spent"] != float64(11) || quota["opus_used_images"] != float64(0) {
		t.Fatalf("paid fallback quota=%v", quota)
	}
}

func TestProjectedEmptyOpusBatteryRequiresOfficialExhaustion(t *testing.T) {
	key := clientKey{AllowFixed: true, FixedLimit: 20}
	cost := jobCost{Full: 11, OpusEligible: true, V5: true}
	q := &quotaSnapshot{Official: upstreamQuota{Tier: 3, Active: true, OpusKnown: true, OpusPercent: 0}, Fixed: 20}
	if _, err := chooseReservation(key, cost, q); err == nil {
		t.Fatal("projected zero battery allowed paid request before official exhaustion")
	}
	q.Official.OpusNegative = true
	hold, err := chooseReservation(key, cost, q)
	if err != nil || hold.Fixed != 11 || hold.Opus != 0 {
		t.Fatalf("officially exhausted battery: hold=%+v, err=%v", hold, err)
	}
}
