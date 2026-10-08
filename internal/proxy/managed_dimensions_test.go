package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func dimensionTestGeneration(width, height int) map[string]any {
	body := fallbackTestGeneration("generate", 28)
	parameters := body["parameters"].(map[string]any)
	parameters["width"], parameters["height"] = width, height
	return body
}

func TestPaidImageDimensionsEstimate(t *testing.T) {
	for _, tc := range []struct {
		width, height int
		formula, hold int64
		opus          bool
	}{
		{1024, 1024, 30, 41, true},
		{1024, 1088, 33, 45, false},
		{1536, 1024, 45, 59, false},
		{448, 2304, 30, 41, true},
		{1024, 3072, 90, 113, false},
		{3072, 1024, 90, 113, false},
		{4096, 768, 90, 113, false},
		{4096, 1024, 120, 149, false},
		{2048, 2048, 120, 149, false},
	} {
		body, _ := json.Marshal(dimensionTestGeneration(tc.width, tc.height))
		cost, err := estimateJob("/ai/generate-image", body, "application/json")
		if err != nil || cost.FormulaAnlas != tc.formula || cost.Full != tc.hold || cost.OpusEligible != tc.opus {
			t.Errorf("%dx%d: cost=%+v err=%v", tc.width, tc.height, cost, err)
		}
	}
	for _, size := range [][2]int{{63, 1024}, {1024, 63}, {0, 1024}, {-64, 1024}, {1024, 4160}, {4160, 1024}, {int(^uint(0) >> 1), 64}, {64, int(^uint(0) >> 1)}} {
		body, _ := json.Marshal(dimensionTestGeneration(size[0], size[1]))
		if _, err := estimateJob("/ai/generate-image", body, "application/json"); err == nil {
			t.Errorf("invalid or overflowing dimensions accepted: %v", size)
		}
	}
}

func TestPaidLongImageRoutesAndAccounting(t *testing.T) {
	f := newFallbackTestProxy(t, 3)
	key, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 2000})
	// The high-step switch does not turn a paid resolution into an official fallback.
	doManaged(t, managedRequest(t, "PUT", f.url+"/admin/accounts/"+f.relayID, testAdminKey, map[string]any{"fallback_high_steps": true}), http.StatusOK)
	body := dimensionTestGeneration(1024, 3072)
	want, _ := json.Marshal(body)
	for _, route := range []string{"/ai/generate-image", "/ai/generate-image-stream", "/image/ai/generate-image", "/image/ai/generate-image-stream"} {
		count := len(f.snapshot())
		req := managedRequest(t, "POST", f.url+route, key, body)
		req.Header.Set("Content-Type", "application/json")
		f.send(t, req, http.StatusOK)
		calls := f.snapshot()
		got := calls[len(calls)-1]
		if len(calls) != count+1 || got.provider != providerNewAPI || got.path != strings.TrimPrefix(route, "/image") || !bytes.Equal(got.body, want) || got.contentType != "application/json" {
			t.Fatal("paid dimensions changed the request or selected the official account")
		}
	}
	// Multipart and durable jobs must use the same validation and preserve the payload.
	multipartBody, contentType := managedMultipartBody(t,
		multipartPart{name: "request", contentType: "application/json", data: want},
		multipartPart{name: "reference", contentType: "image/png", data: []byte{0, 128, 255}},
	)
	req, _ := http.NewRequest("POST", f.url+"/image/ai/generate-image-stream", bytes.NewReader(multipartBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	f.send(t, req, http.StatusOK)
	calls := f.snapshot()
	got := calls[len(calls)-1]
	if got.provider != providerNewAPI || got.path != "/ai/generate-image-stream" || !bytes.Equal(got.body, multipartBody) || got.contentType != contentType {
		t.Fatal("multipart paid dimensions changed the upstream request")
	}
	count := len(calls)
	job := doManaged(t, managedRequest(t, "POST", f.url+"/jobs/ai/generate-image", key, body), http.StatusAccepted)
	deadline := time.Now().Add(2 * time.Second)
	var status map[string]any
	for {
		status = doManaged(t, managedRequest(t, "GET", f.url+"/jobs/"+job["id"].(string), key, nil), http.StatusOK)
		if status["state"] == "done" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status["state"] != "done" || status["upstream_status"] != float64(http.StatusOK) {
		t.Fatalf("durable paid dimensions were rejected: %v", status)
	}
	calls = f.snapshot()
	got = calls[len(calls)-1]
	if len(calls) != count+1 || got.provider != providerNewAPI || !bytes.Equal(got.body, want) {
		t.Fatal("durable paid dimensions were misrouted or changed")
	}
	usage := doManaged(t, managedRequest(t, "GET", f.url+"/quota", key, nil), http.StatusOK)
	if usage["spent_anlas"] != float64(540) || usage["formula_anlas"] != float64(540) || usage["successful_generations"] != float64(6) || usage["opus_used_images"] != float64(0) || usage["pending_anlas"] != float64(0) {
		t.Fatalf("paid dimensions were not billed to the relay device key: %v", usage)
	}
	limited, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 89})
	count = len(calls)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image", limited, body), http.StatusPaymentRequired)
	if len(f.snapshot()) != count {
		t.Fatal("paid dimensions bypassed the key budget")
	}
	// Direct official requests use Anlas above the free area, without requiring Opus.
	officialKey := doManaged(t, managedRequest(t, "POST", f.url+"/admin/keys", testAdminKey, map[string]any{
		"name": "official paid device", "account_id": defaultAccountID, "allow_fixed_anlas": true, "fixed_anlas_limit": 200,
	}), http.StatusCreated)["key"].(string)
	f.send(t, managedRequest(t, "POST", f.url+"/ai/generate-image-stream", officialKey, body), http.StatusOK)
	stored, _ := f.h.store.find(officialKey)
	if stored.FixedSpent != 113 || stored.FormulaAnlas != 90 || stored.OpusUsed != 0 || stored.SuccessfulGenerations != 1 {
		t.Fatal("paid official dimensions incorrectly required or consumed Opus")
	}
	// An upstream 400 remains distinguishable from the proxy's own dimension rejection.
	for _, tc := range []struct {
		body        map[string]any
		correlation string
		message     string
		forwarded   int
	}{
		{dimensionTestGeneration(1024, 4160), "", "unsupported request parameters: generation dimensions", 0},
		{body, "bad-request", "upstream rejected image dimensions", 1},
	} {
		before, _ := f.h.store.find(key)
		count := len(f.snapshot())
		req := managedRequest(t, "POST", f.url+"/ai/generate-image-stream", key, tc.body)
		req.Header.Set("X-Correlation-Id", tc.correlation)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusBadRequest || !bytes.Contains(data, []byte(tc.message)) {
			t.Fatalf("400 origin was not preserved: status=%d body=%s read=%v", response.StatusCode, data, readErr)
		}
		release, err := f.h.enter(context.Background(), "", "400 settlement", -1)
		if err != nil {
			t.Fatal(err)
		}
		release()
		after, _ := f.h.store.find(key)
		if len(f.snapshot()) != count+tc.forwarded || after.FixedSpent != before.FixedSpent || after.FixedPending != before.FixedPending || after.SuccessfulGenerations != before.SuccessfulGenerations {
			t.Fatal("dimension refusal was retried or charged")
		}
	}
}
