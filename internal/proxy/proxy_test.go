package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef"

func testHandler(t *testing.T, image, api string, maxConcurrent int) *Handler {
	t.Helper()
	handler, err := New(Config{
		SharedKey: testKey, MaxConcurrent: maxConcurrent,
		ImageUpstream: image, APIUpstream: api,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func request(t *testing.T, method, endpoint string, body io.Reader) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Plana-Proxy-Key", testKey)
	return r
}

func TestRoutesAndHeaders(t *testing.T) {
	seen := make(chan string, 2)
	image := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Plana-Proxy-Key") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Untrusted") != "" {
			t.Error("proxy-only or untrusted headers reached upstream")
		}
		if r.Header.Get("Authorization") != "Bearer nai-token" {
			t.Error("NovelAI authorization was not forwarded")
		}
		seen <- r.Method + " " + r.URL.Path + " " + string(body)
		w.Header().Set("Set-Cookie", "secret=value")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte{0, 1, 2, 255})
	}))
	defer image.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte("api"))
	}))
	defer api.Close()
	server := httptest.NewServer(testHandler(t, image.URL, api.URL, 8))
	defer server.Close()

	r := request(t, http.MethodPost, server.URL+"/image/ai/generate-image-stream", strings.NewReader("input"))
	r.Header.Set("Authorization", "Bearer nai-token")
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Untrusted", "discard")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusCreated || !bytes.Equal(got, []byte{0, 1, 2, 255}) {
		t.Fatalf("response: status=%d body=%v err=%v", resp.StatusCode, got, err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("unsafe response headers: %v", resp.Header)
	}
	if got := <-seen; got != "POST /ai/generate-image-stream input" {
		t.Fatalf("image route: %s", got)
	}

	r = request(t, http.MethodPost, server.URL+"/api/ai/upscale", strings.NewReader("pixels"))
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-seen; got != "POST /ai/upscale" {
		t.Fatalf("api route: %s", got)
	}
}

func TestAccessAndTargetValidation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected upstream call")
	}))
	defer upstream.Close()
	server := httptest.NewServer(testHandler(t, upstream.URL, upstream.URL, 8))
	defer server.Close()
	cases := []struct {
		name, method, path, key string
		status                  int
	}{
		{"missing key", "GET", "/image/user/subscription", "", 401},
		{"wrong key", "GET", "/image/user/subscription", "wrong", 401},
		{"unknown path", "GET", "/image/user/unknown", testKey, 404},
		{"wrong method", "POST", "/image/user/subscription", testKey, 405},
		{"query", "GET", "/image/user/subscription?token=bad", testKey, 400},
		{"encoded path", "GET", "/image/user/%73ubscription", testKey, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request(t, tc.method, server.URL+tc.path, nil)
			if tc.key == "" {
				r.Header.Del("X-Plana-Proxy-Key")
			} else {
				r.Header.Set("X-Plana-Proxy-Key", tc.key)
			}
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
}

func TestStreamingAndCancellation(t *testing.T) {
	firstSent := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 0, 0, 1, 0x91})
		w.(http.Flusher).Flush()
		close(firstSent)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	server := httptest.NewServer(testHandler(t, upstream.URL, upstream.URL, 8))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := request(t, "POST", server.URL+"/image/ai/generate-image-stream", strings.NewReader("{}"))
	r = r.WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-firstSent
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || !bytes.Equal(buf, []byte{0, 0, 0, 1, 0x91}) {
		t.Fatalf("first streamed frame: %v, %v", buf, err)
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach upstream")
	}
}

func TestBodyLimitAndConcurrency(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	handler := testHandler(t, upstream.URL, upstream.URL, 1)
	server := httptest.NewServer(handler)
	defer server.Close()

	tooLarge := request(t, "POST", server.URL+"/image/ai/upscale", strings.NewReader("x"))
	tooLarge.ContentLength = maxBodyBytes + 1
	tooLargeResponse := httptest.NewRecorder()
	handler.ServeHTTP(tooLargeResponse, tooLarge)
	if tooLargeResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit status = %d", tooLargeResponse.Code)
	}

	firstDone := make(chan error, 1)
	go func() {
		r := request(t, "GET", server.URL+"/image/user/subscription", nil)
		resp, err := http.DefaultClient.Do(r)
		if err == nil {
			resp.Body.Close()
		}
		firstDone <- err
	}()
	<-entered
	second := request(t, "GET", server.URL+"/image/user/subscription", nil)
	response, err := http.DefaultClient.Do(second)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("concurrency response: %d %v", response.StatusCode, response.Header)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestUnknownLengthBodyLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := httptest.NewServer(testHandler(t, upstream.URL, upstream.URL, 8))
	defer server.Close()

	r := request(t, http.MethodPost, server.URL+"/image/ai/encode-vibe", io.LimitReader(zeroReader{}, maxBodyBytes+1))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown-length body status = %d, want 413", resp.StatusCode)
	}
}
