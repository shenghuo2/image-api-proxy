package proxy

import (
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerationHoursWithoutArchive(t *testing.T) {
	image := archiveTestPNG(color.RGBA{G: 255, A: 255})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/subscription" {
			_, _ = io.WriteString(w, `{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`)
			return
		}
		switch r.Header.Get("X-Correlation-Id") {
		case "failed":
			http.Error(w, "upstream failed", http.StatusInternalServerError)
		case "empty":
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write(image)
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
	created := doManaged(t, managedRequest(t, http.MethodPost, server.URL+"/admin/keys", testAdminKey, map[string]any{"name": "usage", "allow_fixed_anlas": true, "fixed_anlas_limit": 500}), http.StatusCreated)
	key := created["key"].(string)
	body := map[string]any{"model": "nai-diffusion-4-5-full", "parameters": map[string]any{"width": 512, "height": 512, "steps": 12, "n_samples": 1}}
	for _, tc := range []struct {
		marker string
		status int
	}{{"good-1", 200}, {"failed", 500}, {"empty", 200}, {"good-2", 200}} {
		request := managedRequest(t, http.MethodPost, server.URL+"/ai/generate-image", key, body)
		request.Header.Set("X-Correlation-Id", tc.marker)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("%s: status=%d", tc.marker, response.StatusCode)
		}
	}
	stats := archiveAdminGet(t, h, "/admin/images/stats")
	var archive struct {
		Count        int  `json:"count"`
		EverArchived bool `json:"ever_archived"`
	}
	if stats.Code != 200 || json.Unmarshal(stats.Body.Bytes(), &archive) != nil || archive.Count != 0 || archive.EverArchived {
		t.Fatalf("disabled archive changed: %d %s", stats.Code, stats.Body.String())
	}
	start := time.Now().Add(-24 * time.Hour)
	end := time.Now().Add(24 * time.Hour)
	params := url.Values{"from": {start.Format(time.RFC3339)}, "to": {end.Format(time.RFC3339)}, "offset_minutes": {"0"}}
	query := "/admin/usage/hours?" + params.Encode()
	check := func(handler *ManagedHandler) {
		t.Helper()
		response := archiveAdminGet(t, handler, query)
		var result struct {
			Hours []usageHour `json:"hours"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Hours) != 1 || result.Hours[0].Count != 2 || result.Hours[0].Generations != 2 {
			t.Fatalf("usage heatmap: %d %s", response.Code, response.Body.String())
		}
	}
	check(h)
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, query, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("usage heatmap leaked: %d", unauthorized.Code)
	}
	for _, target := range []string{"/admin/usage/hours?from=bad", "/admin/usage/hours?" + params.Encode() + "&unknown=1"} {
		if response := archiveAdminGet(t, h, target); response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid usage range: %d", response.Code)
		}
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	check(restarted)
}

func TestUsageQuarterLocalOffset(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: filepath.Join(t.TempDir(), "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 24, 16, 15, 0, 0, time.UTC)
	if err := h.db.recordGeneration(base, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.db.recordGeneration(base.Add(3*time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	params := url.Values{"from": {base.Add(-time.Hour).Format(time.RFC3339)}, "to": {base.Add(time.Hour).Format(time.RFC3339)}, "offset_minutes": {"-480"}}
	response := archiveAdminGet(t, h, "/admin/usage/hours?"+params.Encode())
	var result struct {
		Hours []usageHour `json:"hours"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Hours) != 1 || result.Hours[0].Date != "2026-09-25" || result.Hours[0].Hour != 0 || result.Hours[0].Count != 3 || result.Hours[0].Generations != 2 {
		t.Fatalf("local usage hour: %d %s", response.Code, response.Body.String())
	}
}
