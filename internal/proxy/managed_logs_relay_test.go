package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRequestLogsRelayAndOfficialFallback(t *testing.T) {
	f := newFallbackTestProxy(t, 3)
	t.Cleanup(f.h.BeginDrain)
	key, _ := f.key(t, map[string]any{"allow_fixed_anlas": true, "fixed_anlas_limit": 2000})
	for _, tc := range []struct {
		name, route, correlation, provider, billing string
		body                                        map[string]any
		status                                      int
		reserved                                    int64
	}{
		{"paid relay", "/ai/generate-image", "", providerNewAPI, "charged", dimensionTestGeneration(1024, 3072), 200, 90},
		{"official Max", "/image/ai/generate-image-stream", "", providerNovelAI, "charged", longDimensionTestBody(1024, 2304, "generate", true), 200, 87},
		{"relay refusal", "/ai/generate-image-stream", "bad-request", providerNewAPI, "refunded", dimensionTestGeneration(1024, 3072), 400, 90},
		{"high step refusal", "/ai/generate-image", "", "", "not_reserved", fallbackTestGeneration("enhance", 29), 503, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := managedRequest(t, "POST", f.url+tc.route, key, tc.body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-Id", tc.correlation)
			f.send(t, req, tc.status)
			list, err := f.h.logs.list(context.Background(), requestLogFilter{Limit: 1})
			if err != nil || len(list.Items) != 1 {
				t.Fatalf("request log missing: count=%d err=%v", len(list.Items), err)
			}
			entry := list.Items[0]
			if entry.SourceAccountID != f.relayID || entry.Provider != tc.provider || entry.Status != tc.status || entry.BillingState != tc.billing || entry.ReservedAnlas != tc.reserved {
				t.Fatalf("wrong relay log attribution or accounting: %+v", entry)
			}
			if tc.provider == providerNewAPI {
				account, _ := f.h.accounts.find(f.relayID)
				origin, _ := parseUpstream(account.Origin)
				if entry.AccountID != f.relayID || entry.UpstreamHost != origin.Host {
					t.Fatal("relay logged as an official request")
				}
			} else if tc.provider == providerNovelAI {
				if entry.AccountID != defaultAccountID || !entry.UpscaledEnhance || !entry.Stream || entry.UpstreamHost != f.h.imageURL.Host {
					t.Fatal("official fallback missing from Max Enhance log")
				}
			}
		})
	}
	list, _ := f.h.logs.list(context.Background(), requestLogFilter{Limit: 50})
	if len(list.Items) != 4 {
		t.Fatalf("relay and fallback requests logged more than once: %d", len(list.Items))
	}
	data, _ := json.Marshal(list)
	for _, secret := range []string{key, testAdminKey, testNAIToken, "relay-test-token-0123456789"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("credential leaked into relay request logs")
		}
	}
	files, _ := os.ReadDir(f.h.logs.dir)
	for _, file := range files {
		data, _ := os.ReadFile(filepath.Join(f.h.logs.dir, file.Name()))
		if bytes.Contains(data, []byte(key)) || bytes.Contains(data, []byte("relay-test-token-0123456789")) {
			t.Fatal("relay credential persisted in log file")
		}
	}
}
