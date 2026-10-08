package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func longDimensionTestBody(width, height int, action string, maxEnhance bool) map[string]any {
	parameters := map[string]any{"width": width, "height": height, "steps": 28, "n_samples": 1}
	if action != "generate" || maxEnhance {
		parameters["image"], parameters["strength"] = "source-image", 1.0
	}
	if maxEnhance {
		parameters["upscaled_enhance"] = true
	}
	return map[string]any{"model": "nai-diffusion-5-full", "action": action, "parameters": parameters}
}

func TestGenerationDimensionAreaLimit(t *testing.T) {
	for _, tc := range []struct {
		width, height int
		formula, hold int64
		opus          bool
	}{
		{1024, 1024, 30, 41, true},
		{1024, 1088, 33, 45, false},
		{448, 2304, 30, 41, true},
		{1024, 3072, 90, 113, false},
		{3072, 1024, 90, 113, false},
		{4096, 768, 90, 113, false},
		{4096, 1024, 120, 149, false},
		{2048, 2048, 120, 149, false},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			body, _ := json.Marshal(longDimensionTestBody(tc.width, tc.height, "generate", false))
			cost, err := estimateJob("/ai/generate-image", body, "application/json")
			if err != nil || cost.FormulaAnlas != tc.formula || cost.Full != tc.hold || cost.OpusEligible != tc.opus {
				t.Fatalf("cost=%+v err=%v", cost, err)
			}
		})
	}
	for _, size := range [][2]int{{63, 1024}, {1024, 63}, {0, 1024}, {1024, 0}, {-64, 1024}, {1024, -64}, {1024, 4160}, {4160, 1024}, {int(^uint(0) >> 1), 64}, {64, int(^uint(0) >> 1)}} {
		body, _ := json.Marshal(longDimensionTestBody(size[0], size[1], "generate", false))
		if _, err := estimateJob("/ai/generate-image", body, "application/json"); err == nil {
			t.Errorf("invalid or overflowing dimensions accepted: %v", size)
		}
	}
}

func TestManagedLongGenerationAndEnhance(t *testing.T) {
	image := archiveTestPNG(color.RGBA{G: 255, A: 255})
	frame := archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": image})
	type upstreamCall struct {
		path, contentType string
		body              []byte
	}
	var calls []upstreamCall
	transport := settlementTransport(func(r *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: -1}
		if r.URL.Path == "/user/subscription" {
			response.Body = io.NopCloser(strings.NewReader(`{"active":true,"tier":3,"trainingStepsLeft":{"fixedTrainingStepsLeft":10000,"purchasedTrainingSteps":0},"usage":{"percent":100,"isNegative":false}}`))
			return response, nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if r.Header.Get("Authorization") != "Bearer "+testNAIToken {
			t.Error("generation was sent with the wrong upstream credential")
		}
		calls = append(calls, upstreamCall{r.URL.Path, r.Header.Get("Content-Type"), body})
		if r.Header.Get("X-Correlation-Id") == "dimension-refused" {
			response.StatusCode = http.StatusBadRequest
			response.Body = io.NopCloser(strings.NewReader("upstream rejected image dimensions"))
		} else if strings.HasSuffix(r.URL.Path, "generate-image-stream") {
			response.Header.Set("Content-Type", "application/octet-stream")
			response.Body = io.NopCloser(bytes.NewReader(frame))
		} else {
			response.Header.Set("Content-Type", "image/png")
			response.Body = io.NopCloser(bytes.NewReader(image))
		}
		return response, nil
	})
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken,
		StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: "http://upstream.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	const raw = "long-dimensions-client-key"
	const limited = "long-dimensions-limited-key"
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys,
			clientKey{ID: "long", Name: "long", Hash: hexHash(raw), PolicyVersion: 2, AllowFixed: true, FixedLimit: 10000},
			clientKey{ID: "limited", Name: "limited", Hash: hexHash(limited), PolicyVersion: 2, AllowFixed: true, FixedLimit: 112},
		), nil
	}); err != nil {
		t.Fatal(err)
	}
	var spent, formula, generations int64
	for _, tc := range []struct {
		name, action  string
		width, height int
		maxEnhance    bool
		formula, hold int64
	}{
		{"generate", "generate", 1024, 3072, false, 90, 113},
		{"img2img", "img2img", 3072, 1024, false, 90, 113},
		{"enhance", "enhance", 1024, 2304, false, 68, 87},
		{"max enhance", "generate", 1024, 2304, true, 68, 87},
	} {
		for _, route := range []string{"/ai/generate-image", "/ai/generate-image-stream", "/image/ai/generate-image", "/image/ai/generate-image-stream"} {
			for _, multipart := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/multipart=%t", tc.name, route, multipart), func(t *testing.T) {
					body, _ := json.Marshal(longDimensionTestBody(tc.width, tc.height, tc.action, tc.maxEnhance))
					contentType := "application/json"
					if multipart {
						body, contentType = managedMultipartBody(t,
							multipartPart{name: "request", contentType: "application/json", data: body},
							multipartPart{name: "image", contentType: "image/png", data: image})
					}
					req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+raw)
					req.Header.Set("Content-Type", contentType)
					result := httptest.NewRecorder()
					count := len(calls)
					h.ServeHTTP(result, req)
					want := image
					if strings.HasSuffix(route, "generate-image-stream") {
						want = frame
					}
					if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), want) {
						t.Fatalf("long image rejected or response changed: status=%d body=%q", result.Code, result.Body.Bytes())
					}
					if len(calls) != count+1 {
						t.Fatalf("forwarded %d generation requests, want one", len(calls)-count)
					}
					call := calls[count]
					if call.path != strings.TrimPrefix(route, "/image") || call.contentType != contentType || !bytes.Equal(call.body, body) {
						t.Fatal("long image request changed before reaching upstream")
					}
					spent, formula, generations = spent+tc.hold, formula+tc.formula, generations+1
					stored, _ := h.store.find(raw)
					if stored.FixedSpent != spent || stored.FormulaAnlas != formula || stored.SuccessfulGenerations != generations || stored.SuccessfulImages != generations || stored.FixedPending != 0 || stored.OpusUsed != 0 {
						t.Fatalf("paid long image accounting: spent=%d formula=%d generations=%d pending=%d opus=%d", stored.FixedSpent, stored.FormulaAnlas, stored.SuccessfulGenerations, stored.FixedPending, stored.OpusUsed)
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		name, key, correlation, message string
		height, status, forwarded       int
	}{
		{"local refusal", raw, "", "unsupported request parameters: generation dimensions", 4160, http.StatusBadRequest, 0},
		{"upstream refusal", raw, "dimension-refused", "upstream rejected image dimensions", 3072, http.StatusBadRequest, 1},
		{"insufficient budget", limited, "", "Anlas quota or permission insufficient", 3072, http.StatusPaymentRequired, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := h.store.find(tc.key)
			count := len(calls)
			body, _ := json.Marshal(longDimensionTestBody(1024, tc.height, "generate", false))
			req := httptest.NewRequest(http.MethodPost, "/ai/generate-image-stream", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-Id", tc.correlation)
			result := httptest.NewRecorder()
			h.ServeHTTP(result, req)
			if result.Code != tc.status || !strings.Contains(result.Body.String(), tc.message) {
				t.Fatalf("error origin was not preserved: status=%d body=%q", result.Code, result.Body.String())
			}
			after, _ := h.store.find(tc.key)
			if len(calls) != count+tc.forwarded || after.FixedSpent != before.FixedSpent || after.FixedPending != before.FixedPending || after.OpusUsed != before.OpusUsed || after.FormulaAnlas != before.FormulaAnlas || after.SuccessfulGenerations != before.SuccessfulGenerations {
				t.Fatal("rejected request was retried or charged")
			}
		})
	}
}
