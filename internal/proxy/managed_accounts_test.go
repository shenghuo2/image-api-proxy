package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestManagedAccountPoolAndLegacyBinding(t *testing.T) {
	const secondToken = "nai-server-token-secondary-012345"
	var mu sync.Mutex
	var generated []string
	quotaCalls := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != testNAIToken && token != secondToken {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/user/subscription":
			quotaCalls[token]++
			balance := 100
			if token == secondToken {
				balance = 50
			}
			fmt.Fprintf(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":%d,"purchasedTrainingSteps":0},"usage":{"percent":100}}`, balance)
		case "/ai/generate-image":
			generated = append(generated, token)
			_, _ = w.Write([]byte("image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "keys.json")
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: path, ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	createdAccount := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, map[string]any{"name": "secondary", "token": secondToken}), 201)
	secondID := createdAccount["id"].(string)
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, map[string]any{"name": "duplicate", "token": secondToken}), 409)
	if secondID == defaultAccountID {
		t.Fatal("second account reused default ID")
	}
	if _, ok := createdAccount["token"]; ok {
		t.Fatal("account API disclosed token")
	}
	accountResponse, err := http.DefaultClient.Do(managedRequest(t, "GET", server.URL+"/admin/accounts", testAdminKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	var accountList []publicAccount
	err = json.NewDecoder(accountResponse.Body).Decode(&accountList)
	accountResponse.Body.Close()
	if err != nil || len(accountList) != 2 || !accountList[0].TokenConfigured || !accountList[1].TokenConfigured {
		t.Fatalf("account list: %v, %v", accountList, err)
	}
	data, err := os.ReadFile(path + ".accounts.json")
	if err != nil || strings.Contains(string(data), secondToken) || strings.Contains(string(data), testNAIToken) {
		t.Fatal("account tokens must be encrypted at rest")
	}
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "pool-key", "allow_fixed_anlas": true, "fixed_anlas_limit": 80}), 201)
	key := created["key"].(string)
	if created["client"].(map[string]any)["account_id"] != poolAccountID {
		t.Fatal("new keys must default to the account pool")
	}
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	for range 2 {
		doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job), 200)
	}
	mu.Lock()
	if len(generated) != 2 || generated[0] != testNAIToken || generated[1] != secondToken || quotaCalls[testNAIToken] != 1 || quotaCalls[secondToken] != 1 {
		t.Errorf("pool rotation or quota cache: jobs=%v, refreshes=%v", generated, quotaCalls)
	}
	mu.Unlock()
	fixed := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "fixed-key", "account_id": secondID, "allow_fixed_anlas": true, "fixed_anlas_limit": 10}), 201)
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", fixed["key"].(string), job), 200)
	mu.Lock()
	if generated[len(generated)-1] != secondToken {
		t.Error("fixed key used the wrong account")
	}
	mu.Unlock()
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/accounts/default", testAdminKey, map[string]any{"enabled": false}), 200)
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "fixed-default", "account_id": defaultAccountID, "allow_fixed_anlas": true, "fixed_anlas_limit": 10}), 400)
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", key, job), 200)
	mu.Lock()
	if len(generated) != 4 || generated[3] != secondToken {
		t.Errorf("disabled account used by pool: %v", generated)
	}
	mu.Unlock()
	legacy := "legacy-key-bound-to-default"
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "legacy", Name: "legacy", Hash: hexHash(legacy), PolicyVersion: 1, AllowFixed: true, FixedLimit: 20}), nil
	}); err != nil {
		t.Fatal(err)
	}
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", legacy, job), 503)
	doManaged(t, managedRequest(t, "PUT", server.URL+"/admin/accounts/default", testAdminKey, map[string]any{"enabled": true}), 200)
	view := doManaged(t, managedRequest(t, "GET", server.URL+"/user/subscription", legacy, nil), 200)
	if view["trainingStepsLeft"].(map[string]any)["fixedTrainingStepsLeft"] != float64(20) {
		t.Fatalf("legacy key was not bound to default account: %v", view)
	}
	doManaged(t, managedRequest(t, "DELETE", server.URL+"/admin/accounts/default", testAdminKey, nil), 409)
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path, ImageUpstream: upstream.URL})
	if err != nil || len(restarted.accounts.snapshot()) != 2 {
		t.Fatalf("accounts did not persist: %v", err)
	}
	if token, err := restarted.accountToken(secondID); err != nil || token != secondToken {
		t.Fatal("secondary token not available after restart")
	}
}

func TestAccountPoolSkipsUnavailableQuota(t *testing.T) {
	const secondToken = "nai-server-token-secondary-012345"
	var failedCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			if r.Header.Get("Authorization") == "Bearer "+testNAIToken {
				failedCalls.Add(1)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":0},"usage":{"percent":100}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secondToken {
			t.Error("pool selected unavailable account")
		}
		_, _ = w.Write([]byte("image"))
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, "POST", server.URL+"/admin/accounts", testAdminKey, map[string]any{"name": "working", "token": secondToken}), 201)
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "pool", "allow_fixed_anlas": true, "fixed_anlas_limit": 20}), 201)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	doManaged(t, managedRequest(t, "POST", server.URL+"/ai/generate-image", created["key"].(string), job), 200)
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/admin/quota", testAdminKey, nil), 200)
	if len(quota["account_quotas"].([]any)) != 1 || len(quota["account_errors"].([]any)) != 1 || failedCalls.Load() != 1 {
		t.Fatalf("partial account quota or backoff: %v, failed calls=%d", quota, failedCalls.Load())
	}
}
