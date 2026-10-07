package proxy

import (
	"bytes"
	"context"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type settlementTransport func(*http.Request) (*http.Response, error)

func (f settlementTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type interruptedImageBody struct {
	data   *bytes.Reader
	cancel context.CancelFunc
}

func (b *interruptedImageBody) Read(p []byte) (int, error) {
	if b.data.Len() == 0 {
		b.cancel()
		return 0, io.ErrUnexpectedEOF
	}
	return b.data.Read(p)
}
func (*interruptedImageBody) Close() error { return nil }

type failedImageWriter struct{ *httptest.ResponseRecorder }

func (*failedImageWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStreamSettlementAfterClientCloses(t *testing.T) {
	image := archiveTestPNG(color.RGBA{R: 255, A: 128})
	final := archiveTestFrame(map[string]any{"code": 200, "step_ix": nil, "image": image})
	preview := archiveTestFrame(map[string]any{"code": 200, "step_ix": 1, "image": image})
	rejection := archiveTestFrame(map[string]any{"code": 402, "message": "not enough points"})
	for _, tc := range []struct {
		name        string
		data        []byte
		charge      bool
		failWrite   bool
		interrupted bool
		spent       int64
		pending     int64
		generations int64
	}{
		{"final_then_disconnect", final, false, false, true, 9, 0, 1},
		{"final_downstream_write_fails", final, false, true, false, 9, 0, 1},
		{"preview_then_disconnect", preview, false, false, true, 9, 9, 0},
		{"preview_disconnect_charged", preview, true, false, true, 9, 0, 0},
		{"incomplete_final", final[:len(final)-1], false, false, true, 9, 9, 0},
		{"rejection", rejection, true, false, false, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := settlementTransport(func(r *http.Request) (*http.Response, error) {
				var body io.ReadCloser
				if r.URL.Path == "/user/subscription" {
					body = io.NopCloser(strings.NewReader(`{"active":true,"tier":2,"trainingStepsLeft":{"fixedTrainingStepsLeft":1000,"purchasedTrainingSteps":0}}`))
				} else if tc.interrupted {
					body = &interruptedImageBody{data: bytes.NewReader(tc.data), cancel: cancel}
				} else {
					body = io.NopCloser(bytes.NewReader(tc.data))
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1}, nil
			})
			h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json"), ImageUpstream: "http://upstream.invalid", Transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			settings := h.settings.snapshot()
			settings.ChargePendingAsSpent, settings.ArchiveEnabled = tc.charge, true
			if err := h.settings.setWithAccounting(settings, h.store); err != nil {
				t.Fatal(err)
			}
			const raw = "settlement-client-key"
			if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
				return append(keys, clientKey{ID: "settlement", Name: "test", Hash: hexHash(raw), PolicyVersion: 2, AllowFixed: true, FixedLimit: 500}), nil
			}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/ai/generate-image-stream", strings.NewReader(`{"model":"nai-diffusion-4-5-full","parameters":{"width":512,"height":512,"steps":12,"n_samples":1}}`))
			req.Header.Set("Authorization", "Bearer "+raw)
			req.Header.Set("Content-Type", "application/json")
			// Force ReverseProxy's real HTTP-server abort behavior on a read/write error.
			req = req.WithContext(context.WithValue(ctx, http.ServerContextKey, &http.Server{}))
			var writer http.ResponseWriter = httptest.NewRecorder()
			if tc.failWrite {
				writer = &failedImageWriter{httptest.NewRecorder()}
			}
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				h.ServeHTTP(writer, req)
			}()
			if tc.interrupted || tc.failWrite {
				if panicked != http.ErrAbortHandler {
					t.Fatalf("abort = %v", panicked)
				}
			} else if panicked != nil {
				t.Fatalf("unexpected panic: %v", panicked)
			}
			key := h.store.snapshot()[0]
			if key.FixedSpent != tc.spent || key.FixedPending != tc.pending || key.SuccessfulGenerations != tc.generations || key.SuccessfulImages != tc.generations || key.FormulaAnlas != tc.generations*3 {
				t.Fatalf("accounting: spent=%d pending=%d generations=%d images=%d formula=%d", key.FixedSpent, key.FixedPending, key.SuccessfulGenerations, key.SuccessfulImages, key.FormulaAnlas)
			}
			if h.queueState().Active != nil {
				t.Fatal("aborted request did not release the FIFO")
			}
			if tc.generations > 0 {
				waitArchiveCount(t, h, 1)
			}
		})
	}
}

func TestChargePendingSettingCommitsExistingWithoutDoubleCharge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if h.settings.snapshot().ChargePendingAsSpent {
		t.Fatal("automatic charge must default off")
	}
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "old", FixedSpent: 40, FixedPending: 10, PurchasedSpent: 20, PurchasedPending: 5, OpusUsed: 8, OpusPending: 3, SuccessfulGenerations: 2, OpusBuckets: map[string]opusBucket{"account": {Balance: 1000, Pending: 3}}}), nil
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(handler *ManagedHandler, charge bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"charge_pending_as_spent":`+map[bool]string{true: "true", false: "false"}[charge]+`}`))
		req.Header.Set("Authorization", "Bearer "+testAdminKey)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("settings update: %d %s", w.Code, w.Body.String())
		}
	}
	check := func(handler *ManagedHandler) {
		t.Helper()
		key := handler.store.snapshot()[0]
		if key.FixedSpent != 40 || key.PurchasedSpent != 20 || key.OpusUsed != 8 || key.FixedPending+key.PurchasedPending+key.OpusPending != 0 || key.OpusBuckets["account"].Pending != 0 || key.OpusBuckets["account"].Balance != 1000 || key.SuccessfulGenerations != 2 {
			t.Fatalf("committing pending changed balances or counts: %+v", key)
		}
	}
	apply(h, true)
	apply(h, true)
	check(h)
	// A crash can leave a new reservation on disk while the option is enabled.
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) { keys[0].FixedPending = 4; return keys, nil }); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	check(restarted)
	apply(restarted, false)
	check(restarted)
}

func TestPendingSettingAndAccountingRollbackTogether(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: filepath.Join(t.TempDir(), "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
		return append(keys, clientKey{ID: "pending", FixedSpent: 9, FixedPending: 9}), nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.db.Exec("CREATE TRIGGER reject_keys BEFORE UPDATE ON keys BEGIN SELECT RAISE(ABORT, 'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	settings := h.settings.snapshot()
	settings.ChargePendingAsSpent = true
	if err := h.settings.setWithAccounting(settings, h.store); err == nil {
		t.Fatal("expected transaction failure")
	}
	if h.settings.snapshot().ChargePendingAsSpent || h.store.snapshot()[0].FixedPending != 9 {
		t.Fatal("in-memory settings/keys were partially committed")
	}
	restarted, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, StatePath: h.store.path})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.settings.snapshot().ChargePendingAsSpent || restarted.store.snapshot()[0].FixedPending != 9 {
		t.Fatal("settings/keys were partially persisted")
	}
}
