package proxy

import (
	"bytes"
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewAPIAccountRoutingAndLocalUsage(t *testing.T) {
	const upstreamKey = "new-api-test-token-123456789"
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			t.Error("New API balance placeholder must not be fetched")
			http.Error(w, "unexpected subscription fetch", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
			t.Error("upstream key was not substituted")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Contains(body, []byte(`"model":"nai-diffusion-`)) {
			t.Errorf("request body changed: %s, %v", body, err)
		}
		if r.Header.Get("X-Correlation-Id") == "paid" && (string(body) != `{"model":"nai-diffusion-5-full","parameters":{"width":1536,"height":1024,"steps":35,"n_samples":1}}` || r.Header.Get("Content-Type") != "application/json") {
			t.Errorf("paid-size request changed: body=%s content-type=%q", body, r.Header.Get("Content-Type"))
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/ai/generate-image":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write([]byte("PK\x03\x04image"))
		case "/ai/generate-image-stream":
			if bytes.Contains(body, []byte("nai-diffusion-4-5-full")) {
				http.Error(w, "Not enough Anlas and out of trial image generations. Required: 20, Available: -49977", http.StatusPaymentRequired)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(archiveTestFrame(map[string]any{"image": archiveTestPNG(color.RGBA{R: 255, A: 255})}))
		default:
			t.Errorf("unsupported route forwarded: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	statePath := filepath.Join(t.TempDir(), "keys.json")
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: statePath})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	account := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, map[string]any{
		"name": "relay", "provider": "new_api", "origin": upstream.URL, "token": upstreamKey,
		"enabled_models": []string{"nai-diffusion-4-5-full", "nai-diffusion-5-full"},
	}), http.StatusCreated)
	if account["provider"] != providerNewAPI || account["origin"] != upstream.URL {
		t.Fatalf("account metadata: %v", account)
	}
	accountID := account["id"].(string)
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), http.StatusOK)
	if quota["known_account_count"] != float64(0) || quota["unknown_balance_account_count"] != float64(1) || quota["upstream_anlas"] != float64(0) {
		t.Fatalf("placeholder was reported as official balance: %v", quota)
	}
	view := quota["account_quotas"].([]any)[0].(map[string]any)
	if view["upstream_balance_known"] != false || view["projected_fixed_anlas"] != nil {
		t.Fatalf("relay balance presented as known: %v", view)
	}
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{
		"name": "device A", "account_id": accountID, "allow_fixed_anlas": true, "fixed_anlas_limit": 20,
	}), http.StatusCreated)
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{
		"name": "invalid Opus", "account_id": accountID, "allow_fixed_anlas": true, "fixed_anlas_limit": 20, "allow_opus": true, "opus_limit_images": 1,
	}), http.StatusBadRequest)
	key := created["key"].(string)
	readJob := func(req *http.Request, status int) {
		t.Helper()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != status {
			t.Fatalf("image response: status=%d body=%q read=%v", resp.StatusCode, body, readErr)
		}
	}
	waitUsage := func(rawKey string, generations int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			usage := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", rawKey, nil), http.StatusOK)
			if usage["successful_generations"] == float64(generations) || time.Now().After(deadline) {
				return usage
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	makeJob := func(model string) map[string]any {
		return map[string]any{"model": model, "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	}
	readJob(managedRequest(t, "POST", server.URL+"/ai/generate-image", key, makeJob("nai-diffusion-4-5-full")), http.StatusOK)
	failed, err := http.DefaultClient.Do(managedRequest(t, "POST", server.URL+"/ai/generate-image-stream", key, makeJob("nai-diffusion-4-5-full")))
	if err != nil {
		t.Fatal(err)
	}
	failureBody, _ := io.ReadAll(failed.Body)
	_ = failed.Body.Close()
	if failed.StatusCode != http.StatusPaymentRequired || !bytes.Contains(failureBody, []byte("Required: 20, Available: -49977")) {
		t.Fatalf("stream error was not forwarded: status=%d body=%s", failed.StatusCode, failureBody)
	}
	readJob(managedRequest(t, "POST", server.URL+"/image/ai/generate-image-stream", key, makeJob("nai-diffusion-5-full")), http.StatusOK)
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", key, makeJob("nai-diffusion-4-5-curated")), http.StatusServiceUnavailable)
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/encode-vibe", key, map[string]any{"image": "unused"}), http.StatusServiceUnavailable)
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 3 || gotPaths[0] != "/ai/generate-image" || gotPaths[1] != "/ai/generate-image-stream" || gotPaths[2] != "/ai/generate-image-stream" {
		t.Fatalf("upstream routes: %v", gotPaths)
	}
	usage := waitUsage(key, 2)
	if usage["successful_generations"] != float64(2) || usage["successful_images"] != float64(2) || usage["formula_anlas"] != float64(8) || usage["spent_anlas"] != float64(8) || usage["pending_anlas"] != float64(0) {
		t.Fatalf("local usage after successful and failed generations: %v", usage)
	}
	subscription := doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", key, nil), http.StatusOK)
	if subscription["balance_source"] != "local_budget_upstream_unknown" || subscription["trainingStepsLeft"].(map[string]any)["fixedTrainingStepsLeft"] != float64(12) {
		t.Fatalf("subscription must expose local budget source: %v", subscription)
	}
	paidKey := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{
		"name": "device B", "account_id": accountID, "allow_fixed_anlas": true, "fixed_anlas_limit": 200,
	}), http.StatusCreated)["key"].(string)
	paidBody := `{"model":"nai-diffusion-5-full","parameters":{"width":1536,"height":1024,"steps":35,"n_samples":1}}`
	paidRequest, err := http.NewRequest(http.MethodPost, server.URL+"/ai/generate-image", strings.NewReader(paidBody))
	if err != nil {
		t.Fatal(err)
	}
	paidRequest.Header.Set("Authorization", "Bearer "+paidKey)
	paidRequest.Header.Set("Content-Type", "application/json")
	paidRequest.Header.Set("X-Correlation-Id", "paid")
	readJob(paidRequest, http.StatusOK)
	paidUsage := waitUsage(paidKey, 1)
	if paidUsage["successful_generations"] != float64(1) || paidUsage["formula_anlas"] != float64(56) || paidUsage["spent_anlas"] != float64(56) {
		t.Fatalf("paid-size local usage: %v", paidUsage)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: statePath})
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := restarted.store.find(key)
	if !ok || stored.SuccessfulGenerations != 2 || stored.FormulaAnlas != 8 {
		t.Fatalf("local usage was not persisted: %+v", stored)
	}
	if persisted, ok := restarted.accounts.find(accountID); !ok || persisted.provider() != providerNewAPI || len(persisted.EnabledModels) != 2 {
		t.Fatalf("relay account was not persisted: %+v", persisted)
	}
	var data []byte
	if err := h.db.db.QueryRow("SELECT data FROM accounts WHERE id=?", accountID).Scan(&data); err != nil || strings.Contains(string(data), upstreamKey) {
		t.Fatal("upstream key must remain encrypted in SQLite")
	}
}

func TestNewAPIAccountValidation(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: filepath.Join(t.TempDir(), "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	for _, input := range []map[string]any{
		{"name": "bad path", "provider": "new_api", "origin": "https://relay.example/v1", "token": "test-token-123456789", "enabled_models": []string{"nai-diffusion-5-full"}},
		{"name": "bad model", "provider": "new_api", "origin": "https://relay.example", "token": "test-token-123456789", "enabled_models": []string{"nai-diffusion-3"}},
		{"name": "empty models", "provider": "new_api", "origin": "https://relay.example", "token": "test-token-123456789", "enabled_models": []string{}},
		{"name": "duplicate models", "provider": "new_api", "origin": "https://relay.example", "token": "test-token-123456789", "enabled_models": []string{"nai-diffusion-5-full", "nai-diffusion-5-full"}},
	} {
		doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, input), http.StatusBadRequest)
	}
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, map[string]any{
		"name": "relay", "provider": "new_api", "origin": "https://relay.example", "token": "test-token-123456789", "enabled_models": []string{"nai-diffusion-5-full"},
	}), http.StatusCreated)
	id := created["id"].(string)
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/accounts/"+id, testAdminKey, map[string]any{"provider": "novelai"}), http.StatusBadRequest)
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/accounts/"+id, testAdminKey, map[string]any{"enabled_models": []string{"nai-diffusion-4-5-curated"}}), http.StatusOK)
	view := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/accounts/"+id+"/quota", testAdminKey, nil), http.StatusOK)
	if view["upstream_balance_known"] != false {
		t.Fatalf("account quota: %v", view)
	}
	response, err := http.DefaultClient.Do(managedRequest(t, "GET", server.URL+"/admin/accounts", testAdminKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var accounts []publicAccount
	if err := json.NewDecoder(response.Body).Decode(&accounts); err != nil || len(accounts) != 1 || len(accounts[0].EnabledModels) != 1 || accounts[0].EnabledModels[0] != "nai-diffusion-4-5-curated" {
		t.Fatalf("updated model selection: %+v, %v", accounts, err)
	}
}
