package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnlimitedAndPercentLimits(t *testing.T) {
	k := clientKey{AllowFixed: true, FixedLimit: -1, FixedSpent: 50, AllowPurchased: true, PurchasedLimit: -1, PurchasedSpent: 20, AllowOpus: true, OpusLimit: -1, OpusUsed: 2}
	view := viewKey(k)
	if view.FixedRemaining != -1 || view.PurchasedRemaining != -1 || view.OpusRemaining != -1 || view.RemainingAnlas != -1 {
		t.Fatalf("unlimited balance: %+v", view)
	}
	q := &quotaSnapshot{Fixed: 3, Purchased: 4, Official: upstreamQuota{Tier: 2}}
	hold, err := chooseReservation(k, jobCost{Full: 10}, q)
	if err == nil || hold != (reservation{}) {
		t.Fatalf("upstream balance must cap unlimited key: %+v, %v", hold, err)
	}
	hold, err = chooseReservation(k, jobCost{Full: 7}, q)
	if err != nil || hold.Fixed != 3 || hold.Purchased != 4 {
		t.Fatalf("upstream cap: %+v, %v", hold, err)
	}
	k.OpusLimitMode = "percent"
	k.OpusLimitPercent = 10
	k.OpusUsed = 2
	if opusEffectiveLimit(k) != 173 || viewKey(k).OpusRemaining != 171 {
		t.Fatalf("10 percent should allow 173 cumulative images: %+v", viewKey(k))
	}
	mode := "images"
	if err := applyPolicy(&k, keyPolicyInput{OpusLimitMode: &mode}); err != nil || opusEffectiveLimit(k) != -1 {
		t.Fatalf("switch to unlimited images: %+v, %v", k, err)
	}
}

func TestKeyRevealRotationAndLegacyLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = w.Write([]byte(`{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":100,"purchasedTrainingSteps":100},"usage":{"percent":100}}`))
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
	legacyRaw := "legacy-secret"
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "legacy", Name: "legacy", Hash: hexHash(legacyRaw), PolicyVersion: 1, AllowFixed: true, FixedLimit: 10, FixedSpent: 3}), nil
	}); err != nil {
		t.Fatal(err)
	}
	created := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "new", "allow_fixed_anlas": true, "fixed_anlas_limit": -1, "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 10}), 201)
	raw := created["key"].(string)
	if len(raw) != 28 || !strings.HasPrefix(raw, "pst-") {
		t.Fatalf("key format: %q", raw)
	}
	var upper, lower, digit bool
	for _, ch := range raw[4:] {
		upper = upper || ch >= 'A' && ch <= 'Z'
		lower = lower || ch >= 'a' && ch <= 'z'
		digit = digit || ch >= '0' && ch <= '9'
	}
	if !upper || !lower || !digit {
		t.Fatal("key must contain mixed case and digits")
	}
	var data []byte
	err = h.db.db.QueryRow("SELECT data FROM keys WHERE id=?", created["client"].(map[string]any)["id"]).Scan(&data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), raw) || strings.Contains(string(data), legacyRaw) {
		t.Fatal("plaintext key in ledger")
	}
	list := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	h.ServeHTTP(list, req)
	var keys []publicKey
	if err := json.Unmarshal(list.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Key != "" || keys[1].Key != raw || keys[1].OpusEffectiveLimit != 173 {
		t.Fatalf("admin list: %+v", keys)
	}
	client := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", raw, nil), 200)
	if _, found := client["key"]; found {
		t.Fatal("client quota exposed plaintext key")
	}
	if client["fixed_anlas_remaining"] != float64(-1) || client["opus_effective_limit_images"] != float64(173) {
		t.Fatalf("client quota: %v", client)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	var restored clientKey
	for _, candidate := range restarted.store.snapshot() {
		if candidate.ID == created["client"].(map[string]any)["id"] {
			restored = candidate
		}
	}
	if restarted.viewAdminKey(restored).Key != raw {
		t.Fatal("key unavailable after restart")
	}
	rotated := doManaged(t, managedRequest(t, "POST", server.URL+"/admin/keys/legacy/rotate", testAdminKey, nil), 200)
	newRaw := rotated["key"].(string)
	if len(newRaw) != 28 || !strings.HasPrefix(newRaw, "pst-") {
		t.Fatal("rotated key format")
	}
	doManaged(t, managedRequest(t, "GET", server.URL+"/quota", legacyRaw, nil), 401)
	current := doManaged(t, managedRequest(t, "GET", server.URL+"/quota", newRaw, nil), 200)
	if current["fixed_anlas_spent"] != float64(3) || current["id"] != "legacy" {
		t.Fatalf("rotation lost usage: %v", current)
	}
}
