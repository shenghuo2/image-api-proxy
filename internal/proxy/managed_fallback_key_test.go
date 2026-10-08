package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayHighStepsKeyPermissionsAndPersistence(t *testing.T) {
	f := newFallbackTestProxy(t, 3)
	t.Cleanup(f.h.BeginDrain)
	blocked, blockedID := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 500})
	allowed, allowedID := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 500, "allow_fallback_high_steps": true})
	for _, tc := range []struct {
		key     string
		allowed bool
	}{{blocked, false}, {allowed, true}} {
		quota := doManaged(t, managedRequest(t, "GET", f.url+"/quota", tc.key, nil), http.StatusOK)
		if quota["allow_fallback_high_steps"] != tc.allowed {
			t.Fatal("client quota did not expose the independent key permission")
		}
		if _, exists := quota["key"]; exists {
			t.Fatal("client quota exposed the credential")
		}
	}
	// Key authorization can be prepared before the account's master switch is enabled.
	updated := doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+allowedID, testAdminKey, map[string]any{"name": "authorized device"}), http.StatusOK)
	if updated["allow_fallback_high_steps"] != true {
		t.Fatal("an unrelated edit cleared key authorization")
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_high_steps": true}), http.StatusOK)
	body := fallbackTestGeneration("enhance", 29)
	before, _ := f.h.store.find(blocked)
	count := len(f.snapshot())
	// A flag in a client's generation body cannot grant administrative permission.
	body["allow_fallback_high_steps"] = true
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", blocked, body), http.StatusServiceUnavailable)
	after, _ := f.h.store.find(blocked)
	if len(f.snapshot()) != count || after.FixedSpent != before.FixedSpent || after.FixedPending != before.FixedPending || after.SuccessfulGenerations != before.SuccessfulGenerations {
		t.Fatal("a denied key reached upstream or changed its usage")
	}
	logs, err := f.h.logs.list(context.Background(), requestLogFilter{KeyID: blockedID, Limit: 1})
	if err != nil || len(logs.Items) != 1 || logs.Items[0].BillingState != "not_reserved" || logs.Items[0].ReservedAnlas != 0 {
		t.Fatal("key permission refusal was not logged without a reservation")
	}
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", allowed, body), http.StatusOK)
	if calls := f.snapshot(); len(calls) != count+1 || calls[len(calls)-1].provider != providerNovelAI {
		t.Fatal("authorized key did not use its official fallback")
	}
	updated = doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+allowedID, testAdminKey, map[string]any{"allow_fallback_high_steps": false}), http.StatusOK)
	if updated["allow_fallback_high_steps"] != false {
		t.Fatal("key authorization could not be revoked")
	}
	count = len(f.snapshot())
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", allowed, body), http.StatusServiceUnavailable)
	if len(f.snapshot()) != count {
		t.Fatal("revoked key authorization still reached the official fallback")
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+allowedID, testAdminKey, map[string]any{"allow_fallback_high_steps": true}), http.StatusOK)
	// Simulate a pre-upgrade SQLite key record with no new permission field.
	data, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "allow_fallback_high_steps")
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.store.db.db.Exec("UPDATE keys SET data=? WHERE id=?", data, blockedID); err != nil {
		t.Fatal(err)
	}
	f.h.BeginDrain()
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: f.state, ImageUpstream: f.h.imageURL.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.BeginDrain)
	for _, tc := range []struct {
		key     string
		allowed bool
	}{{blocked, false}, {allowed, true}} {
		stored, ok := restarted.store.find(tc.key)
		if !ok || stored.AllowFallbackHighSteps != tc.allowed {
			t.Fatal("restart changed explicit or legacy key authorization")
		}
	}
	server := httptest.NewServer(restarted)
	t.Cleanup(server.Close)
	quota := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", blocked, nil), http.StatusOK)
	if quota["allow_fallback_high_steps"] != false {
		t.Fatal("legacy key inherited the account's high-step permission")
	}
}

func TestRelayHighStepsKeyPermissionsInPool(t *testing.T) {
	f := newFallbackTestProxy(t, 2)
	t.Cleanup(f.h.BeginDrain)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_high_steps": true}), http.StatusOK)
	cost := jobCost{Model: "nai-diffusion-5-full", Steps: 29}
	for _, allowed := range []bool{false, true} {
		key := clientKey{AccountID: poolAccountID, AllowFallbackHighSteps: allowed}
		candidates := f.h.accountCandidatesForJob(key, "/ai/generate-image", cost)
		want := 2
		if allowed {
			want = 1
		}
		if len(candidates) != want {
			t.Fatalf("pool key authorization selected the wrong candidates: allowed=%v candidates=%+v", allowed, candidates)
		}
		seen := make(map[string]bool)
		for _, account := range candidates {
			if seen[account.ID] || allowed && account.provider() != providerNovelAI {
				t.Fatal("pool duplicated or misrouted the authorized official fallback")
			}
			seen[account.ID] = true
		}
	}
}
