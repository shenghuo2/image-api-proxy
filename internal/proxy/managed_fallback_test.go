package proxy

import (
	"bytes"
	"context"
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

type fallbackTestCall struct {
	provider, path, contentType string
	body                        []byte
}

type fallbackTestProxy struct {
	h                   *ManagedHandler
	url, relayID, state string
	mu                  sync.Mutex
	calls               []fallbackTestCall
}

func newFallbackTestProxy(t *testing.T, tier int) *fallbackTestProxy {
	t.Helper()
	f := &fallbackTestProxy{state: filepath.Join(t.TempDir(), "keys.json")}
	upstream := func(provider, token string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("wrong upstream credential")
			}
			if r.URL.Path == "/user/subscription" {
				if provider != providerNovelAI {
					t.Error("relay balance was queried")
				}
				jsonReply(w, http.StatusOK, map[string]any{"active": true, "tier": tier,
					"trainingStepsLeft": map[string]int{"fixedTrainingStepsLeft": 10000, "purchasedTrainingSteps": 1000},
					"usage":             map[string]any{"percent": 100, "isNegative": false}})
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.calls = append(f.calls, fallbackTestCall{provider, r.URL.Path, r.Header.Get("Content-Type"), body})
			f.mu.Unlock()
			switch r.Header.Get("X-Correlation-Id") {
			case "reject":
				http.Error(w, "upstream refused", http.StatusPaymentRequired)
				return
			case "uncertain":
				http.Error(w, "upstream interrupted", http.StatusBadGateway)
				return
			}
			png := archiveTestPNG(color.RGBA{R: 255, A: 255})
			if strings.HasSuffix(r.URL.Path, "generate-image-stream") {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(archiveTestFrame(map[string]any{"image": png}))
			} else {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(png)
			}
		}))
	}
	official := upstream(providerNovelAI, testNAIToken)
	relay := upstream(providerNewAPI, "relay-test-token-0123456789")
	t.Cleanup(official.Close)
	t.Cleanup(relay.Close)
	var err error
	f.h, err = NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: f.state, ImageUpstream: official.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.h)
	t.Cleanup(server.Close)
	f.url = server.URL
	account := doManaged(t, managedRequest(t, "POST", f.url+"/admin/accounts", testAdminKey, map[string]any{
		"name": "relay", "provider": providerNewAPI, "origin": relay.URL, "token": "relay-test-token-0123456789",
		"enabled_models": []string{"nai-diffusion-4-5-full", "nai-diffusion-5-full"}, "fallback_account_id": defaultAccountID,
	}), http.StatusCreated)
	f.relayID = account["id"].(string)
	return f
}

func (f *fallbackTestProxy) key(t *testing.T, policy map[string]any) (string, string) {
	t.Helper()
	policy["name"], policy["account_id"] = "device", f.relayID
	created := doManaged(t, managedRequest(t, "POST", f.url+"/admin/keys", testAdminKey, policy), http.StatusCreated)
	return created["key"].(string), created["client"].(map[string]any)["id"].(string)
}

func fallbackTestGeneration(action string, steps int) map[string]any {
	return map[string]any{"model": "nai-diffusion-5-full", "action": action, "use_new_shared_trial": true,
		"parameters": map[string]any{"width": 512, "height": 512, "steps": steps, "n_samples": 1}}
}

func (f *fallbackTestProxy) send(t *testing.T, req *http.Request, status int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != status {
		t.Fatalf("%s: status=%d want=%d body=%q err=%v", req.URL.Path, resp.StatusCode, status, data, err)
	}
	// Response bytes can arrive just before settlement; join the FIFO before checking it.
	release, err := f.h.enter(context.Background(), "", "test settlement", -1)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func (f *fallbackTestProxy) snapshot() []fallbackTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fallbackTestCall(nil), f.calls...)
}

func TestRelayFallbackRoutingAndAccounting(t *testing.T) {
	f := newFallbackTestProxy(t, 2)
	key, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 2000})
	text := fallbackTestGeneration("generate", 12)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, text), http.StatusOK)
	image := fallbackTestGeneration("img2img", 12)
	image["parameters"].(map[string]any)["image"] = "test-image"
	image["parameters"].(map[string]any)["strength"] = 0.5
	f.send(t, managedRequest(t, "POST", f.url+"/image/ai/generate-image", key, image), http.StatusOK)
	upscale := map[string]any{"image": "test-image", "model": "nai-diffusion-5-full", "declared_blur_sigma": 0}
	f.send(t, managedRequest(t, "POST", f.url+"/ai/upscale", key, upscale), http.StatusOK)
	usage := doManaged(t, managedRequest(t, "GET", f.url+"/quota", key, nil), http.StatusOK)
	// Relay generation costs 5; official img2img reserves 9; standalone upscale reserves 200.
	if usage["spent_anlas"] != float64(214) || usage["pending_anlas"] != float64(0) || usage["formula_anlas"] != float64(8) || usage["successful_generations"] != float64(2) || usage["account_id"] != f.relayID {
		t.Fatalf("wrong fallback attribution or pricing: %v", usage)
	}
	calls := f.snapshot()
	if len(calls) != 3 || calls[0].provider != providerNewAPI || calls[1].provider != providerNovelAI || calls[2].provider != providerNovelAI {
		t.Fatalf("wrong account routing: %+v", calls)
	}

	for _, tc := range []struct {
		name, route, provider string
		body                  map[string]any
	}{
		{"reference only", "/ai/generate-image", providerNewAPI, fallbackTestGeneration("generate", 12)},
		{"primary image", "/ai/generate-image-stream", providerNovelAI, fallbackTestGeneration("generate", 12)},
		{"top level image", "/image/ai/generate-image-stream", providerNovelAI, fallbackTestGeneration("generate", 12)},
		{"inpainting", "/ai/generate-image", providerNovelAI, fallbackTestGeneration("infill", 12)},
		{"enhance", "/ai/generate-image", providerNovelAI, fallbackTestGeneration("enhance", 12)},
		{"max enhance", "/ai/generate-image-stream", providerNovelAI, fallbackTestGeneration("generate", 12)},
		{"vibe", "/image/ai/encode-vibe", providerNovelAI, map[string]any{"image": "test-image"}},
		{"director", "/image/ai/augment-image", providerNovelAI, map[string]any{"image": "test-image", "req_type": "colorize", "width": 512, "height": 512}},
		{"upscale alias", "/image/ai/upscale", providerNovelAI, upscale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := tc.body["parameters"].(map[string]any)
			switch tc.name {
			case "reference only":
				p["reference_image_multiple"] = []string{"vibe-image"}
				p["director_reference_images"] = []string{"reference-image"}
			case "primary image":
				p["image"] = "test-image"
			case "top level image":
				tc.body["image"] = "test-image"
			case "inpainting":
				tc.body["model"] = "nai-diffusion-5-full-inpainting"
				p["image"], p["mask"] = "test-image", "test-mask"
			case "max enhance":
				p["upscaled_enhance"] = true
			}
			want, _ := json.Marshal(tc.body)
			req := managedRequest(t, "POST", f.url+tc.route, key, tc.body)
			req.Header.Set("Content-Type", "application/json")
			f.send(t, req, http.StatusOK)
			calls := f.snapshot()
			got := calls[len(calls)-1]
			if got.provider != tc.provider || got.path != strings.TrimPrefix(tc.route, "/image") || !bytes.Equal(got.body, want) || got.contentType != "application/json" {
				t.Fatalf("request changed or misrouted: %+v", got)
			}
		})
	}
	request, _ := json.Marshal(image)
	body, contentType := managedMultipartBody(t,
		multipartPart{name: "request", contentType: "application/json", data: request},
		multipartPart{name: "test-image", contentType: "image/png", data: []byte{0, 255, 128}},
	)
	req, _ := http.NewRequest("POST", f.url+"/image/ai/generate-image-stream", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	f.send(t, req, http.StatusOK)
	calls = f.snapshot()
	got := calls[len(calls)-1]
	if got.provider != providerNovelAI || got.path != "/ai/generate-image-stream" || !bytes.Equal(got.body, body) || got.contentType != contentType {
		t.Fatalf("multipart fallback changed request: %+v", got)
	}

	// A disabled model and insufficient local budget do not grant access to the fallback.
	blocked := fallbackTestGeneration("img2img", 12)
	blocked["model"] = "nai-diffusion-4-5-curated"
	before := len(f.snapshot())
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, blocked), http.StatusServiceUnavailable)
	limited, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 5})
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", limited, image), http.StatusPaymentRequired)
	if len(f.snapshot()) != before {
		t.Fatal("blocked request reached an upstream")
	}
	for _, tc := range []struct {
		body map[string]any
		id   string
		code int
	}{
		{text, "reject", http.StatusPaymentRequired},
		{image, "reject", http.StatusPaymentRequired},
		{image, "uncertain", http.StatusBadGateway},
	} {
		old, _ := f.h.store.find(key)
		req := managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, tc.body)
		req.Header.Set("X-Correlation-Id", tc.id)
		before := len(f.snapshot())
		f.send(t, req, tc.code)
		current, _ := f.h.store.find(key)
		if len(f.snapshot()) != before+1 || current.SuccessfulGenerations != old.SuccessfulGenerations {
			t.Fatal("failed request retried or counted as successful")
		}
		if tc.id == "reject" && (current.FixedSpent != old.FixedSpent || current.FixedPending != old.FixedPending) {
			t.Fatal("explicit upstream refusal was charged")
		}
		if tc.id == "uncertain" && (current.FixedSpent != old.FixedSpent+9 || current.FixedPending != old.FixedPending+9) {
			t.Fatal("uncertain official result did not keep its reservation")
		}
	}
}

func TestRelayFallbackConfigurationAndPersistence(t *testing.T) {
	f := newFallbackTestProxy(t, 2)
	key, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 100})
	for _, id := range []string{f.relayID, poolAccountID, "missing"} {
		doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_account_id": id}), http.StatusBadRequest)
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/default", testAdminKey, map[string]any{"fallback_account_id": f.relayID}), http.StatusBadRequest)
	doManaged(t, managedRequest(t, "DELETE", f.url+"/admin/accounts/default", testAdminKey, nil), http.StatusConflict)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/default", testAdminKey, map[string]any{"enabled": false}), http.StatusOK)
	image := fallbackTestGeneration("img2img", 12)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, image), http.StatusServiceUnavailable)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, fallbackTestGeneration("generate", 12)), http.StatusOK)
	// Other edits preserve a configured but temporarily disabled fallback.
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"name": "renamed", "fallback_account_id": defaultAccountID}), http.StatusOK)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/default", testAdminKey, map[string]any{"enabled": true}), http.StatusOK)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"enabled": false}), http.StatusOK)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, image), http.StatusServiceUnavailable)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"enabled": true}), http.StatusOK)
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: f.state})
	if err != nil {
		t.Fatal(err)
	}
	account, _ := restarted.accounts.find(f.relayID)
	if account.FallbackAccountID != defaultAccountID || restarted.viewAccount(upstreamAccount{ID: defaultAccountID}, nil).FallbackReferenceCount != 1 {
		t.Fatal("fallback reference was not persisted")
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_account_id": ""}), http.StatusOK)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, image), http.StatusServiceUnavailable)
	doManaged(t, managedRequest(t, "DELETE", f.url+"/admin/accounts/default", testAdminKey, nil), http.StatusNoContent)
}

func TestRelayFallbackOpusPermissionAndAccountShares(t *testing.T) {
	f := newFallbackTestProxy(t, 3)
	key, keyID := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 100})
	image := fallbackTestGeneration("img2img", 12)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, image), http.StatusPaymentRequired)
	if len(f.snapshot()) != 0 {
		t.Fatal("Opus was used without permission")
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+keyID, testAdminKey, map[string]any{
		"allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 30,
	}), http.StatusOK)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, image), http.StatusOK)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, fallbackTestGeneration("generate", 12)), http.StatusOK)
	usage := doManaged(t, managedRequest(t, "GET", f.url+"/quota", key, nil), http.StatusOK)
	if usage["opus_remaining_images"] != float64(518) || usage["opus_used_images"] != float64(1) || usage["spent_anlas"] != float64(5) || usage["formula_anlas"] != float64(10) || usage["successful_generations"] != float64(2) {
		t.Fatalf("fallback Opus and relay budget were mixed: %v", usage)
	}
	stored, _ := f.h.store.find(key)
	if len(stored.OpusBuckets) != 1 || stored.OpusBuckets[defaultAccountID].Balance != 518*opusUnit {
		t.Fatalf("wrong account Opus bucket: %+v", stored.OpusBuckets)
	}
	// Relay shares count toward the same official account's 100% allocation ceiling.
	doManaged(t, managedRequest(t, "POST", f.url+"/admin/keys", testAdminKey, map[string]any{
		"name": "official", "account_id": defaultAccountID, "allow_opus": true, "opus_limit_mode": "percent", "opus_limit_percent": 60,
	}), http.StatusCreated)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+keyID, testAdminKey, map[string]any{"opus_limit_percent": 50}), http.StatusConflict)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_account_id": ""}), http.StatusConflict)
	// Refund belongs to the actual official account, and an uncertain result stays there.
	for _, id := range []string{"reject", "uncertain"} {
		req := managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, image)
		req.Header.Set("X-Correlation-Id", id)
		code := http.StatusPaymentRequired
		if id == "uncertain" {
			code = http.StatusBadGateway
		}
		f.send(t, req, code)
	}
	stored, _ = f.h.store.find(key)
	if stored.OpusUsed != 2 || stored.OpusPending != 1 || stored.OpusBuckets[defaultAccountID].Pending != 1 || stored.OpusBuckets[defaultAccountID].Balance != 517*opusUnit {
		t.Fatalf("fallback settlement was not attributed to official account: %+v", stored.OpusBuckets)
	}
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/keys/"+keyID, testAdminKey, map[string]any{"allow_opus": false}), http.StatusOK)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_account_id": ""}), http.StatusConflict)
	doManaged(t, managedRequest(t, "POST", f.url+"/admin/keys/"+keyID+"/reconcile", testAdminKey, map[string]any{
		"charged_anlas": 0, "opus_charged_images": 0, "opus_charged_by_account": map[string]int{defaultAccountID: 0},
	}), http.StatusOK)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_account_id": ""}), http.StatusOK)
}

func TestHighStepsPermissionAppliesBeforeRoutingAndToDurableJobs(t *testing.T) {
	f := newFallbackTestProxy(t, 2)
	key, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 1000})
	if !f.h.settings.snapshot().AllowHighSteps {
		t.Fatal("existing high-step behavior must be retained by default")
	}
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, fallbackTestGeneration("generate", 50)), http.StatusOK)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, fallbackTestGeneration("generate", 51)), http.StatusBadRequest)
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/settings", testAdminKey, map[string]any{"allow_high_steps": false}), http.StatusOK)
	before := len(f.snapshot())
	for _, route := range []string{"/ai/generate-image", "/ai/generate-image-stream", "/image/ai/generate-image", "/image/ai/generate-image-stream"} {
		for _, action := range []string{"generate", "img2img"} {
			f.send(t, managedRequest(t, "POST", f.url+route, key, fallbackTestGeneration(action, 29)), http.StatusPaymentRequired)
		}
	}
	payload, _ := json.Marshal(fallbackTestGeneration("img2img", 29))
	body, contentType := managedMultipartBody(t, multipartPart{name: "request", contentType: "application/json", data: payload})
	req, _ := http.NewRequest("POST", f.url+"/ai/generate-image-stream", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	f.send(t, req, http.StatusPaymentRequired)
	job := doManaged(t, managedRequest(t, "POST", f.url+"/jobs/ai/generate-image", key, fallbackTestGeneration("img2img", 29)), http.StatusAccepted)
	deadline := time.Now().Add(2 * time.Second)
	var status map[string]any
	for {
		status = doManaged(t, managedRequest(t, "GET", f.url+"/jobs/"+job["id"].(string), key, nil), http.StatusOK)
		if status["state"] == "done" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status["state"] != "done" || status["upstream_status"] != float64(http.StatusPaymentRequired) || len(f.snapshot()) != before {
		t.Fatalf("high steps bypassed restriction: job=%v calls=%d", status, len(f.snapshot()))
	}
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", key, fallbackTestGeneration("generate", 28)), http.StatusOK)
	stored, _ := f.h.store.find(key)
	if stored.SuccessfulGenerations != 2 || stored.FixedPending != 0 {
		t.Fatal("blocked high-step requests changed accounting")
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: f.state})
	if err != nil || restarted.settings.snapshot().AllowHighSteps {
		t.Fatalf("high-step switch did not persist: %v", err)
	}
}
