package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type ManagedConfig struct {
	AdminKey      string
	AdminOrigin   string
	NovelAIToken  string
	StatePath     string
	QueueSize     int
	QuotaTTL      time.Duration
	ImageUpstream string
	Transport     http.RoundTripper
}

type ticket struct {
	ctx   context.Context
	ready chan struct{}
	done  chan struct{}
}

type ManagedHandler struct {
	adminHash        [sha256.Size]byte
	adminOrigin      string
	token            string
	store            *keyStore
	vault            keyVault
	settings         *settingsStore
	queue            chan ticket
	client           *http.Client
	imageURL         *url.URL
	image            *httputil.ReverseProxy
	quotaTTL         time.Duration
	quota            *quotaSnapshot
	lastQuotaAttempt time.Time
}

func NewManaged(cfg ManagedConfig) (*ManagedHandler, error) {
	if len(cfg.AdminKey) < 32 || (cfg.NovelAIToken != "" && len(cfg.NovelAIToken) < 16) {
		return nil, errors.New("admin key must be at least 32 characters and configured NovelAI token at least 16")
	}
	if cfg.AdminOrigin != "" {
		origin, err := url.Parse(cfg.AdminOrigin)
		if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil || origin.String() != cfg.AdminOrigin {
			return nil, errors.New("admin origin must be an exact http(s) origin")
		}
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 64
	}
	if cfg.QueueSize < 1 || cfg.QueueSize > 10000 {
		return nil, errors.New("queue size must be between 1 and 10000")
	}
	if cfg.QuotaTTL == 0 {
		cfg.QuotaTTL = 5 * time.Minute
	}
	if cfg.QuotaTTL < time.Second {
		return nil, errors.New("quota TTL must be at least one second")
	}
	if cfg.ImageUpstream == "" {
		cfg.ImageUpstream = "https://image.novelai.net"
	}
	imageURL, err := parseUpstream(cfg.ImageUpstream)
	if err != nil {
		return nil, fmt.Errorf("image upstream: %w", err)
	}
	store, err := openKeyStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	settings, err := openSettingsStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 6 * time.Minute
		t.DisableCompression = true
		t.MaxIdleConnsPerHost = 1
		transport = t
	}
	h := &ManagedHandler{
		adminHash:   sha256.Sum256([]byte(cfg.AdminKey)),
		adminOrigin: cfg.AdminOrigin,
		token:       cfg.NovelAIToken,
		store:       store,
		vault:       newKeyVault(cfg.AdminKey),
		settings:    settings,
		queue:       make(chan ticket, cfg.QueueSize),
		client:      &http.Client{Transport: transport, Timeout: 25 * time.Second},
		imageURL:    imageURL,
		image:       newReverseProxy(imageURL, "/image", transport),
		quotaTTL:    cfg.QuotaTTL,
	}
	go h.runQueue()
	return h, nil
}

func (h *ManagedHandler) runQueue() {
	for t := range h.queue {
		if t.ctx.Err() != nil {
			close(t.ready)
			continue
		}
		close(t.ready)
		<-t.done
	}
}

func (h *ManagedHandler) enter(ctx context.Context) (func(), error) {
	t := ticket{ctx: ctx, ready: make(chan struct{}), done: make(chan struct{})}
	select {
	case h.queue <- t:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("queue full")
	}
	select {
	case <-t.ready:
		return func() { close(t.done) }, nil
	case <-ctx.Done():
		// If admission raced with cancellation, the worker still needs a release.
		go func() { <-t.ready; close(t.done) }()
		return nil, ctx.Err()
	}
}

func (h *ManagedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "unsupported request target", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	}
	if r.URL.EscapedPath() != r.URL.Path || r.URL.RawQuery != "" {
		http.Error(w, "unsupported request target", http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		if h.adminOrigin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if h.adminOrigin != "" && r.Header.Get("Origin") == h.adminOrigin {
			w.Header().Set("Access-Control-Allow-Origin", h.adminOrigin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		key, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare(h.adminHash[:], hashKey(key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.serveAdmin(w, r)
		return
	}
	keyToken, ok := bearerToken(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key, ok := h.store.find(keyToken)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/quota" {
		h.serveClientQuota(w, r, key)
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/user/subscription" || r.URL.Path == "/image/user/subscription") {
		h.serveSubscription(w, r, key)
		return
	}
	if r.URL.Path == "/user/login" || r.URL.Path == "/image/user/login" {
		http.NotFound(w, r)
		return
	}
	var selected *route
	var allowedMethod string
	for i := range routes {
		if routes[i].path == r.URL.Path {
			allowedMethod = routes[i].method
			if routes[i].method == r.Method {
				selected = &routes[i]
			}
			break
		}
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
	h.serveJob(w, r, key, selected)
}

type upstreamQuota struct {
	Fixed        int64
	Purchased    int64
	Tier         int
	Active       bool
	Grace        bool
	Usage        json.RawMessage
	OpusPercent  float64
	OpusKnown    bool
	OpusNegative bool
}

type quotaSnapshot struct {
	Official             upstreamQuota
	Fixed                int64
	Purchased            int64
	OpusJobsSinceRefresh int64
	Refreshed            time.Time
}

func (q *quotaSnapshot) projectedOpusPercent() float64 {
	return math.Max(0, q.Official.OpusPercent-float64(q.OpusJobsSinceRefresh)*100/opusFullImages)
}

// currentQuota is called only while the FIFO worker owns the request.
func (h *ManagedHandler) currentQuota(ctx context.Context, force bool) (*quotaSnapshot, error) {
	now := time.Now()
	if h.quota != nil && now.Sub(h.quota.Refreshed) < h.quotaTTL && !force {
		return h.quota, nil
	}
	refreshFloor := min(30*time.Second, h.quotaTTL)
	if force && h.quota != nil && now.Sub(h.quota.Refreshed) < refreshFloor {
		return h.quota, nil
	}
	if !h.lastQuotaAttempt.IsZero() && now.Sub(h.lastQuotaAttempt) < refreshFloor {
		return nil, errors.New("quota refresh backoff")
	}
	h.lastQuotaAttempt = now
	quota, err := h.fetchQuota(ctx)
	if err != nil {
		slog.Warn("upstream quota refresh failed", "error", err)
		return nil, err
	}
	h.quota = &quotaSnapshot{Official: quota, Fixed: quota.Fixed, Purchased: quota.Purchased, Refreshed: time.Now()}
	return h.quota, nil
}

func (h *ManagedHandler) fetchQuota(ctx context.Context) (upstreamQuota, error) {
	if h.token == "" {
		return upstreamQuota{}, errors.New("NovelAI token is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.imageURL.String()+"/user/subscription", nil)
	if err != nil {
		return upstreamQuota{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return upstreamQuota{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return upstreamQuota{}, fmt.Errorf("subscription returned %d", resp.StatusCode)
	}
	var data struct {
		Active bool `json:"active"`
		Grace  bool `json:"isGracePeriod"`
		Tier   int  `json:"tier"`
		Steps  *struct {
			Fixed     *int64 `json:"fixedTrainingStepsLeft"`
			Purchased *int64 `json:"purchasedTrainingSteps"`
		} `json:"trainingStepsLeft"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return upstreamQuota{}, err
	}
	if data.Steps == nil || data.Steps.Fixed == nil || data.Steps.Purchased == nil || *data.Steps.Fixed < 0 || *data.Steps.Purchased < 0 || *data.Steps.Fixed > 1e9 || *data.Steps.Purchased > 1e9 {
		return upstreamQuota{}, errors.New("invalid upstream balance")
	}
	quota := upstreamQuota{Fixed: *data.Steps.Fixed, Purchased: *data.Steps.Purchased,
		Tier: data.Tier, Active: data.Active, Grace: data.Grace, Usage: data.Usage}
	var usage struct {
		Percent  *float64 `json:"percent"`
		Negative bool     `json:"isNegative"`
	}
	if len(data.Usage) > 0 && json.Unmarshal(data.Usage, &usage) == nil && usage.Percent != nil && !math.IsNaN(*usage.Percent) {
		quota.OpusPercent = *usage.Percent
		quota.OpusKnown = true
		quota.OpusNegative = usage.Negative
	}
	return quota, nil
}

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *ManagedHandler) serveClientQuota(w http.ResponseWriter, r *http.Request, key clientKey) {
	raw, _ := bearerToken(r)
	current, ok := h.store.find(raw)
	if !ok || current.ID != key.ID {
		http.Error(w, "key unavailable", http.StatusUnauthorized)
		return
	}
	for _, latest := range h.store.snapshot() {
		if latest.ID == key.ID && latest.Hash == current.Hash && !latest.Revoked {
			view := viewKey(latest)
			view.QueueLength = len(h.queue)
			jsonReply(w, http.StatusOK, view)
			return
		}
	}
	http.Error(w, "key unavailable", http.StatusUnauthorized)
}

func (h *ManagedHandler) serveSubscription(w http.ResponseWriter, r *http.Request, key clientKey) {
	release, err := h.enter(r.Context())
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	raw, _ := bearerToken(r)
	current, ok := h.store.find(raw)
	if !ok || current.ID != key.ID {
		http.Error(w, "key unavailable", http.StatusUnauthorized)
		return
	}
	q, err := h.currentQuota(r.Context(), false)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	for _, latest := range h.store.snapshot() {
		if latest.ID == key.ID && latest.Hash == current.Hash && !latest.Revoked {
			jsonReply(w, http.StatusOK, map[string]any{
				"active": q.Official.Active, "isGracePeriod": q.Official.Grace,
				"tier": q.Official.Tier,
				"trainingStepsLeft": map[string]int64{
					"fixedTrainingStepsLeft": min(fixedRemaining(latest), q.Fixed),
					"purchasedTrainingSteps": min(purchasedRemaining(latest), q.Purchased),
				},
				"usage": projectedUsage(q, opusRemaining(latest)),
			})
			return
		}
	}
	http.Error(w, "key unavailable", http.StatusUnauthorized)
}

func (h *ManagedHandler) queueError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	w.Header().Set("Retry-After", "2")
	http.Error(w, "queue full", http.StatusTooManyRequests)
}

func (h *ManagedHandler) serveJob(w http.ResponseWriter, r *http.Request, key clientKey, selected *route) {
	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	release, err := h.enter(r.Context())
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	raw, _ := bearerToken(r)
	currentKey, ok := h.store.find(raw)
	if !ok || currentKey.ID != key.ID {
		http.Error(w, "key unavailable", http.StatusUnauthorized)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Minute))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(body) > int(maxBodyBytes) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	cost, err := estimateJob(selected.path, body, r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "unsupported request parameters", http.StatusBadRequest)
		return
	}
	if cost.MultiImage && !h.settings.snapshot().AllowMultiImage {
		http.Error(w, "multi-image disabled in settings", http.StatusPaymentRequired)
		return
	}
	var current clientKey
	found := false
	for _, candidate := range h.store.snapshot() {
		if candidate.ID == key.ID && candidate.Hash == currentKey.Hash && !candidate.Revoked {
			current, found = candidate, true
			break
		}
	}
	if !found || (cost.MultiImage && !current.AllowMultiImage) {
		http.Error(w, "multi-image unavailable for key", http.StatusPaymentRequired)
		return
	}
	q, err := h.currentQuota(r.Context(), false)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	hold, err := chooseReservation(current, cost, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusPaymentRequired)
		return
	}
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == key.ID && keys[i].Hash == currentKey.Hash && !keys[i].Revoked && (!cost.MultiImage || (h.settings.snapshot().AllowMultiImage && keys[i].AllowMultiImage)) && fixedRemaining(keys[i]) >= hold.Fixed && purchasedRemaining(keys[i]) >= hold.Purchased && opusRemaining(keys[i]) >= hold.Opus {
				keys[i].FixedSpent += hold.Fixed
				keys[i].FixedPending += hold.Fixed
				keys[i].PurchasedSpent += hold.Purchased
				keys[i].PurchasedPending += hold.Purchased
				keys[i].OpusUsed += hold.Opus
				keys[i].OpusPending += hold.Opus
				return keys, nil
			}
		}
		return nil, errors.New("client quota insufficient or key revoked")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusPaymentRequired)
		return
	}
	q.Fixed -= hold.Fixed
	q.Purchased -= hold.Purchased
	if cost.V5 {
		q.OpusJobsSinceRefresh += hold.Opus
	}
	upstreamRequest := r.Clone(r.Context())
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Authorization", "Bearer "+h.token)
	upstreamRequest.Body = io.NopCloser(bytes.NewReader(body))
	upstreamRequest.ContentLength = int64(len(body))
	tracked := &statusWriter{ResponseWriter: w}
	h.image.ServeHTTP(tracked, upstreamRequest)
	if tracked.status < 200 || (tracked.status >= 300 && tracked.status < 400) || tracked.status >= 500 || r.Context().Err() != nil {
		return // The upstream outcome is uncertain; keep the reservation pending.
	}
	refund := tracked.status >= 400
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == key.ID {
				keys[i].FixedPending -= hold.Fixed
				keys[i].PurchasedPending -= hold.Purchased
				keys[i].OpusPending -= hold.Opus
				if refund {
					keys[i].FixedSpent -= hold.Fixed
					keys[i].PurchasedSpent -= hold.Purchased
					keys[i].OpusUsed -= hold.Opus
				}
				break
			}
		}
		return keys, nil
	})
	if err == nil && refund {
		q.Fixed += hold.Fixed
		q.Purchased += hold.Purchased
		if cost.V5 {
			q.OpusJobsSinceRefresh -= hold.Opus
		}
	}
}

func projectedUsage(q *quotaSnapshot, opusImages int64) json.RawMessage {
	if len(q.Official.Usage) == 0 || opusImages == 0 {
		return nil
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(q.Official.Usage, &usage); err != nil || !q.Official.OpusKnown {
		return q.Official.Usage
	}
	value := q.projectedOpusPercent()
	if opusImages > 0 {
		value = min(value, float64(opusImages)*100/opusFullImages)
	}
	percent, _ := json.Marshal(value)
	usage["percent"] = percent
	if value == 0 {
		usage["isNegative"] = json.RawMessage("true")
	}
	data, err := json.Marshal(usage)
	if err != nil {
		return q.Official.Usage
	}
	return data
}

func hexHash(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}
