package proxy

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

const maxBodyBytes int64 = 64 << 20

type route struct {
	method string
	path   string
}

// The root paths mirror image.novelai.net. The /image aliases retain the
// base URL used by clients that already include an /image prefix.
var routes = []route{
	{"POST", "/ai/generate-image-stream"},
	{"POST", "/ai/generate-image"},
	{"POST", "/ai/upscale"},
	{"POST", "/ai/encode-vibe"},
	{"GET", "/user/subscription"},
	{"POST", "/image/ai/generate-image-stream"},
	{"POST", "/image/ai/generate-image"},
	{"POST", "/image/ai/upscale"},
	{"POST", "/image/ai/encode-vibe"},
	{"GET", "/image/user/subscription"},
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

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}

func hashKey(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return hash[:]
}
