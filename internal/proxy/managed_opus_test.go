package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpusPercentBucketsRechargeAndPersist(t *testing.T) {
	var official atomic.Int64
	official.Store(55)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/subscription":
			fmt.Fprintf(w, `{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":0,"purchasedTrainingSteps":0},"usage":{"percent":%d,"isNegative":false,"timeUntilNextPercent":3600}}`, official.Load())
		case "/ai/generate-image":
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
	keys := make([]string, 3)
	for index := range keys {
		created := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{
			"name": fmt.Sprintf("share-%d", index), "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 33,
		}), http.StatusCreated)
		keys[index] = created["key"].(string)
	}
	read := func(key string) map[string]any {
		return doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/quota", key, nil), http.StatusOK)
	}
	before := read(keys[0])["opus_remaining_images"].(float64)
	if before < 300 || before > 320 {
		t.Fatalf("unexpected initial share: %v", before)
	}
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", keys[0], job), http.StatusOK)
	if first, second, third := read(keys[0])["opus_remaining_images"].(float64), read(keys[1])["opus_remaining_images"].(float64), read(keys[2])["opus_remaining_images"].(float64); first != before-1 || second != before || third != before {
		t.Fatalf("one key affected other balances: %v, %v, %v", first, second, third)
	}
	official.Store(56)
	h.quotas[defaultAccountID].Refreshed = time.Now().Add(-time.Hour)
	h.lastQuotaAttempt[defaultAccountID] = time.Time{}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/quota/refresh", testAdminKey, nil), http.StatusOK)
	refilled := read(keys[1])["opus_remaining_images"].(float64)
	if refilled <= before || read(keys[0])["opus_remaining_images"].(float64) != refilled-1 {
		t.Fatalf("recharge was not divided independently: %+v %+v", read(keys[0]), read(keys[1]))
	}
	state := h.store.opusSnapshot()[defaultAccountID]
	if _, err := h.predictOpusAccount(defaultAccountID, state.NextPercentAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if predicted := read(keys[1]); !predicted["opus_predicted"].(bool) || predicted["opus_remaining_images"].(float64) <= refilled {
		t.Fatalf("next 1%% prediction missing: %v", predicted)
	}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", keys[1], job), http.StatusOK)
	h.quotas[defaultAccountID].Refreshed = time.Now().Add(-time.Hour)
	h.lastQuotaAttempt[defaultAccountID] = time.Time{}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/quota/refresh", testAdminKey, nil), http.StatusOK)
	if corrected := read(keys[1]); corrected["opus_predicted"].(bool) || corrected["opus_remaining_images"].(float64) != refilled-1 {
		t.Fatalf("unconfirmed prediction not corrected: %v", corrected)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path, ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.store.find(keys[1])
	if !ok {
		t.Fatal("key did not survive restart")
	}
	if loaded.OpusBuckets[defaultAccountID].Balance/opusUnit != int64(refilled-1) {
		t.Fatalf("Opus balance did not persist: %+v", loaded.OpusBuckets)
	}
	official.Store(46)
	h.quotas[defaultAccountID].Refreshed = time.Now().Add(-time.Hour)
	h.lastQuotaAttempt[defaultAccountID] = time.Time{}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/quota/refresh", testAdminKey, nil), http.StatusOK)
	if external := read(keys[1])["opus_remaining_images"].(float64); external >= refilled-1 || external != read(keys[0])["opus_remaining_images"].(float64) {
		t.Fatalf("external spend was not shared after free capacity: %v", external)
	}
}

func TestOpusSharePriorityAndLegacyOvercommit(t *testing.T) {
	keys := []clientKey{
		{ID: "fixed", AccountID: "a", AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 50},
		{ID: "pool", AccountID: poolAccountID, AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 33},
	}
	if got := opusShare(keys, keys[1], "a"); got < .1649 || got > .1651 {
		t.Fatalf("pool should receive 33%% of the unreserved half: %v", got)
	}
	if got := opusShare(keys, keys[1], "b"); got < .3299 || got > .3301 {
		t.Fatalf("pool should receive 33%% of unrestricted account: %v", got)
	}
	accounts := []upstreamAccount{{ID: "a"}, {ID: "b"}}
	keys = append(keys, clientKey{ID: "other", AccountID: "a", AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 60})
	if err := validateOpusShares(keys[:2], keys, accounts); err == nil {
		t.Fatal("new overcommit accepted")
	}
	if err := validateOpusShares(keys, keys, accounts); err != nil {
		t.Fatalf("legacy overcommit should remain readable: %v", err)
	}
	if !opusConfiguredOvercommit(keys, keys[0]) || opusShare(keys, keys[0], "a") >= .5 {
		t.Fatal("legacy overcommit was not normalized and flagged")
	}
}

func TestOpusSharesProtectLegacyModeAndReconcile(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/subscription":
			_, _ = w.Write([]byte(`{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":0,"purchasedTrainingSteps":0},"usage":{"percent":55,"isNegative":false}}`))
		case "/ai/generate-image":
			w.WriteHeader(int(status.Load()))
			_, _ = w.Write([]byte("image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	share := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "reserved", "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 100}), http.StatusCreated)["key"].(string)
	legacy := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "legacy", "allow_opus": true, "opus_limit_images": -1}), http.StatusCreated)["key"].(string)
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", legacy, job), http.StatusPaymentRequired)
	read := func() map[string]any {
		return doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/quota", share, nil), http.StatusOK)
	}
	initial := read()["opus_remaining_images"].(float64)
	status.Store(http.StatusBadRequest)
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", share, job), http.StatusBadRequest)
	if after := read(); after["opus_remaining_images"] != initial || after["opus_pending_images"] != float64(0) {
		t.Fatalf("failed request was not refunded: %v", after)
	}
	status.Store(http.StatusServiceUnavailable)
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", share, job), http.StatusServiceUnavailable)
	pending := read()
	if pending["opus_remaining_images"] != initial-1 || pending["opus_pending_images"] != float64(1) || pending["opus_pending_by_account"].(map[string]any)[defaultAccountID] != float64(1) {
		t.Fatalf("uncertain request did not retain account reservation: %v", pending)
	}
	id := pending["id"].(string)
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys/"+id+"/reconcile", testAdminKey, map[string]any{"charged_anlas": 0, "opus_charged_images": 0, "opus_charged_by_account": map[string]int{defaultAccountID: 0}}), http.StatusOK)
	if after := read(); after["opus_remaining_images"] != initial || after["opus_pending_images"] != float64(0) {
		t.Fatalf("manual reconciliation did not refund correct account: %v", after)
	}
}

func TestOpusLegacyPercentFirstSnapshot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = w.Write([]byte(`{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":0,"purchasedTrainingSteps":0},"usage":{"percent":55}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	raw := "legacy-percent-key"
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "legacy-percent", Name: "legacy", AccountID: defaultAccountID, Hash: hexHash(raw), PolicyVersion: 1, AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 10, OpusUsed: 100, OpusPending: 1}), nil
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/user/subscription", raw, nil), http.StatusOK)
	view := doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/quota", raw, nil), http.StatusOK)
	if view["opus_remaining_images"] != float64(73) || view["opus_used_images"] != float64(100) || view["opus_pending_images"] != float64(1) {
		t.Fatalf("legacy percent balance migrated incorrectly: %v", view)
	}
}

func TestOpusPoolUsesAccountBucketsAndNewTokenBalance(t *testing.T) {
	const secondToken = "nai-server-token-secondary-012345"
	const replacementToken = "nai-server-token-replaced-012345"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/subscription":
			percent := 55
			switch r.Header.Get("Authorization") {
			case "Bearer " + secondToken:
				percent = 75
			case "Bearer " + replacementToken:
				percent = 25
			}
			fmt.Fprintf(w, `{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":0,"purchasedTrainingSteps":0},"usage":{"percent":%d}}`, percent)
		case "/ai/generate-image":
			_, _ = w.Write([]byte("image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	second := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/accounts", testAdminKey, map[string]any{"name": "second", "token": secondToken}), http.StatusCreated)["id"].(string)
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "fixed", "account_id": defaultAccountID, "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 50}), http.StatusCreated)
	pool := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "pool", "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 33}), http.StatusCreated)["key"].(string)
	before, _ := h.store.find(pool)
	first := before.OpusBuckets[defaultAccountID].Balance / opusUnit
	other := before.OpusBuckets[second].Balance / opusUnit
	if first < 150 || first > 160 || other < 420 || other > 435 {
		t.Fatalf("pool did not use remaining fixed share per account: %d, %d", first, other)
	}
	job := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", pool, job), http.StatusOK)
	afterFirst, _ := h.store.find(pool)
	if afterFirst.OpusBuckets[defaultAccountID].Balance != before.OpusBuckets[defaultAccountID].Balance-opusUnit || afterFirst.OpusBuckets[second].Balance != before.OpusBuckets[second].Balance {
		t.Fatalf("first pool request charged wrong account: %+v", afterFirst.OpusBuckets)
	}
	doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", pool, job), http.StatusOK)
	afterSecond, _ := h.store.find(pool)
	if afterSecond.OpusBuckets[second].Balance != before.OpusBuckets[second].Balance-opusUnit {
		t.Fatalf("second pool request charged wrong account: %+v", afterSecond.OpusBuckets)
	}
	doManaged(t, managedRequest(t, http.MethodPut, server.URL+"/admin/accounts/"+second, testAdminKey, map[string]any{"token": replacementToken}), http.StatusOK)
	doManaged(t, managedRequest(t, http.MethodGet, server.URL+"/admin/quota", testAdminKey, nil), http.StatusOK)
	afterReplacement, _ := h.store.find(pool)
	if got := afterReplacement.OpusBuckets[second].Balance / opusUnit; got < 140 || got > 145 || afterReplacement.OpusUsed != 2 {
		t.Fatalf("replacement account did not reseed while preserving usage: %+v", afterReplacement)
	}
}

func TestLegacyPoolPercentSplitsByAvailableCapacity(t *testing.T) {
	keys := []clientKey{{ID: "legacy", AccountID: poolAccountID, PolicyVersion: 1, AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 10, OpusUsed: 50}}
	states := map[string]opusAccountState{
		"a": {Projected: opusUnits(55), ConfirmedAt: time.Now()},
	}
	accounts := []upstreamAccount{{ID: "a"}, {ID: "b"}}
	rebalanceOpusBuckets(keys, states, accounts)
	if keys[0].PolicyVersion != 1 || len(keys[0].OpusBuckets) != 0 {
		t.Fatalf("legacy pool seeded before every account was known: %+v", keys[0])
	}
	states["b"] = opusAccountState{Projected: opusUnits(25), ConfirmedAt: time.Now()}
	rebalanceOpusBuckets(keys, states, accounts)
	first, second := keys[0].OpusBuckets["a"].Balance, keys[0].OpusBuckets["b"].Balance
	if !keys[0].OpusBuckets["a"].Seeded || !keys[0].OpusBuckets["b"].Seeded || first <= second || first+second > 123*opusUnit || keys[0].PolicyVersion != 2 {
		t.Fatalf("legacy pool balance was not split by available capacity: %+v", keys[0])
	}
}

func TestIncreasingOpusShareCreditsOnlyAvailableCapacity(t *testing.T) {
	before := []clientKey{{ID: "a", AccountID: "account", PolicyVersion: 2, AllowOpus: true, OpusLimitMode: "percent", OpusLimitPercent: 25,
		OpusBuckets: map[string]opusBucket{"account": {Seeded: true, Balance: opusUnits(10)}}}}
	after := cloneKeys(before)
	after[0].OpusLimitPercent = 50
	states := map[string]opusAccountState{"account": {Projected: opusUnits(55), ConfirmedAt: time.Now()}}
	accounts := []upstreamAccount{{ID: "account"}}
	rebalanceOpusBuckets(after, states, accounts)
	topUpIncreasedOpusShare(before, after, states, accounts, "a")
	if after[0].OpusBuckets["account"].Balance != opusUnits(23.75) {
		t.Fatalf("increased share did not receive its available fraction: %+v", after[0].OpusBuckets)
	}
	topUpIncreasedOpusShare(after, after, states, accounts, "a")
	if after[0].OpusBuckets["account"].Balance != opusUnits(23.75) {
		t.Fatal("unchanged share received a second credit")
	}
}
