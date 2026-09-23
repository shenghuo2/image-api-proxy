package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const maxBodyBytes int64 = 64 << 20

type Config struct {
	SharedKey     string
	MaxConcurrent int
	// Upstream URLs and Transport are override points for isolated tests.
	ImageUpstream string
	APIUpstream   string
	Transport     http.RoundTripper
}

type route struct {
	method string
	path   string
	group  string
}

var routes = []route{
	{"POST", "/image/ai/generate-image-stream", "image"},
	{"POST", "/image/ai/generate-image", "image"},
	{"POST", "/image/ai/upscale", "image"},
	{"POST", "/image/ai/encode-vibe", "image"},
	{"GET", "/image/user/subscription", "image"},
	{"POST", "/image/user/login", "image"},
	{"POST", "/api/ai/upscale", "api"},
}

type Handler struct {
	keyHash    [sha256.Size]byte
	capacity   chan struct{}
	imageProxy *httputil.ReverseProxy
	apiProxy   *httputil.ReverseProxy
}

func New(cfg Config) (*Handler, error) {
	if len(cfg.SharedKey) < 32 {
		return nil, errors.New("shared key must contain at least 32 characters")
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 8
	}
	if cfg.MaxConcurrent < 1 {
		return nil, errors.New("max concurrent requests must be positive")
	}
	if cfg.ImageUpstream == "" {
		cfg.ImageUpstream = "https://image.novelai.net"
	}
	if cfg.APIUpstream == "" {
		cfg.APIUpstream = "https://api.novelai.net"
	}
	imageURL, err := parseUpstream(cfg.ImageUpstream)
	if err != nil {
		return nil, fmt.Errorf("image upstream: %w", err)
	}
	apiURL, err := parseUpstream(cfg.APIUpstream)
	if err != nil {
		return nil, fmt.Errorf("API upstream: %w", err)
	}
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 6 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
		}
	}
	return &Handler{
		keyHash:    sha256.Sum256([]byte(cfg.SharedKey)),
		capacity:   make(chan struct{}, cfg.MaxConcurrent),
		imageProxy: newReverseProxy(imageURL, "/image", transport),
		apiProxy:   newReverseProxy(apiURL, "/api", transport),
	}, nil
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("expected an http(s) origin without a path, query or credentials")
	}
	return u, nil
}

func newReverseProxy(target *url.URL, prefix string, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, prefix)
			p.Out.URL.RawPath = ""
			p.SetURL(target)

			// Neither the proxy key nor client-controlled forwarding headers go
			// to NovelAI. Preserve only the headers used by the App's requests.
			p.Out.Header = make(http.Header)
			for _, name := range []string{
				"Authorization", "Content-Type", "Accept", "Accept-Language",
				"Origin", "Referer", "User-Agent", "X-Correlation-Id", "X-Initiated-At",
			} {
				if values := p.In.Header.Values(name); len(values) > 0 {
					p.Out.Header[name] = append([]string(nil), values...)
				}
			}
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.URL.RawQuery == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	}

	keys := r.Header.Values("X-Plana-Proxy-Key")
	if len(keys) != 1 || subtle.ConstantTimeCompare(h.keyHash[:], hashKey(keys[0])) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Exact escaped paths reject encoded slashes, path traversal, and queries.
	if r.URL.RawQuery != "" || r.URL.EscapedPath() != r.URL.Path {
		http.Error(w, "unsupported request target", http.StatusBadRequest)
		return
	}
	var selected *route
	var allowedMethod string
	for i := range routes {
		if r.URL.Path != routes[i].path {
			continue
		}
		allowedMethod = routes[i].method
		if r.Method == allowedMethod {
			selected = &routes[i]
		}
		break
	}
	if selected == nil {
		if allowedMethod != "" {
			w.Header().Set("Allow", allowedMethod)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case h.capacity <- struct{}{}:
		defer func() { <-h.capacity }()
	default:
		w.Header().Set("Retry-After", "2")
		http.Error(w, "proxy busy", http.StatusTooManyRequests)
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	if selected.group == "image" {
		h.imageProxy.ServeHTTP(w, r)
	} else {
		h.apiProxy.ServeHTTP(w, r)
	}
}

func hashKey(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return hash[:]
}
