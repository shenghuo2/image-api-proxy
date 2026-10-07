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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type ManagedConfig struct {
	AdminKey          string
	AdminOrigin       string
	NovelAIToken      string
	StatePath         string
	QueueSize         int
	QuotaTTL          time.Duration
	ImageUpstream     string
	Transport         http.RoundTripper
	TrustedProxyCIDRs []string
	LoopbackHostOnly  bool
}

type ManagedHandler struct {
	adminHash        [sha256.Size]byte
	adminOrigin      string
	store            *keyStore
	accounts         *accountStore
	vault            keyVault
	settings         *settingsStore
	jobs             *jobStore
	db               *stateDB
	archive          *archiveManager
	trustedProxies   []*net.IPNet
	jobStaging       chan struct{}
	queueMu          sync.Mutex
	queueSize        int
	draining         bool
	waiting          []*ticket
	active           *ticket
	client           *http.Client
	transport        http.RoundTripper
	imageURL         *url.URL
	image            *httputil.ReverseProxy
	quotaTTL         time.Duration
	quotas           map[string]*quotaSnapshot
	lastQuotaAttempt map[string]time.Time
	poolCursor       int
}

func (h *ManagedHandler) AdminUIPath() string {
	return h.settings.snapshot().AdminUIPath
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
	trusted, err := parseTrustedProxies(cfg.TrustedProxyCIDRs, cfg.LoopbackHostOnly)
	if err != nil {
		return nil, err
	}
	vault := newKeyVault(cfg.AdminKey)
	migrated, err := databaseMigrated(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	var store *keyStore
	var settings *settingsStore
	var accounts *accountStore
	var jobs *jobStore
	if migrated {
		store = &keyStore{path: cfg.StatePath, keys: []clientKey{}}
		settings = &settingsStore{path: cfg.StatePath + ".settings.json"}
		accounts = &accountStore{path: cfg.StatePath + ".accounts.json", accounts: []upstreamAccount{}}
		jobs = &jobStore{dir: cfg.StatePath + ".jobs", jobs: make(map[string]durableJob)}
		if err := os.MkdirAll(jobs.dir, 0700); err != nil {
			return nil, err
		}
	} else {
		store, err = openKeyStore(cfg.StatePath)
		if err != nil {
			return nil, err
		}
		settings, err = openSettingsStore(cfg.StatePath)
		if err != nil {
			return nil, err
		}
		accounts, err = openAccountStore(cfg.StatePath, cfg.NovelAIToken, vault)
		if err != nil {
			return nil, err
		}
		jobs, err = openJobStore(cfg.StatePath + ".jobs")
		if err != nil {
			return nil, err
		}
	}
	db, err := openStateDB(cfg.StatePath, store, accounts, settings, jobs, vault)
	if err != nil {
		return nil, err
	}
	archive, err := newArchiveManager(db, cfg.StatePath, settings)
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
		adminHash:        sha256.Sum256([]byte(cfg.AdminKey)),
		adminOrigin:      cfg.AdminOrigin,
		store:            store,
		accounts:         accounts,
		vault:            vault,
		settings:         settings,
		jobs:             jobs,
		db:               db,
		archive:          archive,
		trustedProxies:   trusted,
		jobStaging:       make(chan struct{}, 4),
		queueSize:        cfg.QueueSize,
		client:           &http.Client{Transport: transport, Timeout: 25 * time.Second},
		transport:        transport,
		imageURL:         imageURL,
		image:            newReverseProxy(imageURL, "/image", transport),
		quotaTTL:         cfg.QuotaTTL,
		quotas:           make(map[string]*quotaSnapshot),
		lastQuotaAttempt: make(map[string]time.Time),
	}
	if err := h.restoreJobs(); err != nil {
		return nil, fmt.Errorf("restore jobs: %w", err)
	}
	return h, nil
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
	if r.URL.EscapedPath() != r.URL.Path || (r.URL.RawQuery != "" && r.URL.Path != "/admin/images" && r.URL.Path != "/admin/images/ips" && r.URL.Path != "/admin/images/overview" && r.URL.Path != "/admin/usage/hours") {
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
	if strings.HasPrefix(r.URL.Path, "/jobs/") {
		h.serveJobAPI(w, r, key)
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
	Fixed              int64
	Purchased          int64
	Tier               int
	Active             bool
	Grace              bool
	Usage              json.RawMessage
	OpusPercent        float64
	OpusKnown          bool
	OpusNegative       bool
	NextPercentSeconds int64
}

type quotaSnapshot struct {
	Official             upstreamQuota
	UnknownBalance       bool
	Fixed                int64
	Purchased            int64
	OpusJobsSinceRefresh int64
	OpusProjected        *float64
	OpusPredicted        bool
	OpusNextPercentAt    time.Time
	Refreshed            time.Time
}

func (q *quotaSnapshot) projectedOpusPercent() float64 {
	if q.Official.Tier != 3 || (!q.Official.Active && !q.Official.Grace) {
		return 0
	}
	if q.OpusProjected != nil {
		return *q.OpusProjected
	}
	return math.Max(0, q.Official.OpusPercent-float64(q.OpusJobsSinceRefresh)*100/opusFullImages)
}

// currentQuota is called only while the FIFO worker owns the request.
func (h *ManagedHandler) currentQuota(ctx context.Context, accountID string, force bool) (*quotaSnapshot, error) {
	account, ok := h.accounts.find(accountID)
	if !ok || account.Disabled {
		return nil, errors.New("upstream account unavailable")
	}
	token, err := h.vault.open(account.TokenCiphertext)
	if err != nil {
		return nil, err
	}
	if account.provider() == providerNewAPI {
		return &quotaSnapshot{UnknownBalance: true,
			Official: upstreamQuota{Active: true, Tier: 3}, Refreshed: time.Now()}, nil
	}
	now := time.Now()
	if q := h.quotas[accountID]; q != nil && now.Sub(q.Refreshed) < h.quotaTTL && !force {
		if state, err := h.predictOpusAccount(accountID, now); err != nil {
			return nil, err
		} else if state != nil {
			projected := float64(state.Projected) * 100 / float64(opusFullUnits)
			q.OpusProjected = &projected
			q.OpusPredicted = state.Predicted
			q.OpusNextPercentAt = state.NextPercentAt
		}
		return q, nil
	}
	refreshFloor := min(30*time.Second, h.quotaTTL)
	if q := h.quotas[accountID]; force && q != nil && now.Sub(q.Refreshed) < refreshFloor {
		return q, nil
	}
	if attempt := h.lastQuotaAttempt[accountID]; !attempt.IsZero() && now.Sub(attempt) < refreshFloor {
		return nil, errors.New("quota refresh backoff")
	}
	h.lastQuotaAttempt[accountID] = now
	quota, err := h.fetchQuota(ctx, token)
	if err != nil {
		slog.Warn("upstream quota refresh failed", "error", err)
		return nil, err
	}
	refreshed := time.Now()
	state, err := h.syncOpusAccount(accountID, token, quota, refreshed)
	if err != nil {
		return nil, err
	}
	h.quotas[accountID] = &quotaSnapshot{Official: quota, Fixed: quota.Fixed, Purchased: quota.Purchased, Refreshed: refreshed}
	if state != nil {
		projected := float64(state.Projected) * 100 / float64(opusFullUnits)
		h.quotas[accountID].OpusProjected = &projected
		h.quotas[accountID].OpusPredicted = state.Predicted
		h.quotas[accountID].OpusNextPercentAt = state.NextPercentAt
	}
	return h.quotas[accountID], nil
}

func (h *ManagedHandler) accountToken(accountID string) (string, error) {
	account, ok := h.accounts.find(accountID)
	if !ok || account.Disabled {
		return "", errors.New("upstream account unavailable")
	}
	return h.vault.open(account.TokenCiphertext)
}

func (h *ManagedHandler) accountCandidates(k clientKey) []upstreamAccount {
	accounts := h.accounts.snapshot()
	available := make([]upstreamAccount, 0, len(accounts))
	for _, account := range accounts {
		if !account.Disabled && (keyAccountID(k) == poolAccountID || keyAccountID(k) == account.ID) {
			available = append(available, account)
		}
	}
	return available
}

func (h *ManagedHandler) accountCandidatesForJob(k clientKey, path, model string) []upstreamAccount {
	accounts := h.accountCandidates(k)
	available := make([]upstreamAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.supports(path, model) {
			available = append(available, account)
		}
	}
	return available
}

// Pool views combine projected balances while the job itself uses one account.
func (h *ManagedHandler) quotaForKey(ctx context.Context, k clientKey) (*quotaSnapshot, error) {
	accounts := h.accountCandidates(k)
	if len(accounts) == 0 {
		return nil, errors.New("no enabled upstream account")
	}
	if keyAccountID(k) != poolAccountID {
		return h.currentQuota(ctx, accounts[0].ID, false)
	}
	combined := &quotaSnapshot{}
	combined.Official.OpusNegative = true
	var firstErr error
	for _, account := range accounts {
		q, err := h.currentQuota(ctx, account.ID, false)
		if err != nil {
			firstErr = err
			continue
		}
		if q.UnknownBalance {
			combined.UnknownBalance = true
			continue
		}
		combined.Fixed += q.Fixed
		combined.Purchased += q.Purchased
		combined.Official.Fixed += q.Official.Fixed
		combined.Official.Purchased += q.Official.Purchased
		combined.Official.Active = combined.Official.Active || q.Official.Active
		combined.Official.Grace = combined.Official.Grace || q.Official.Grace
		combined.Official.Tier = max(combined.Official.Tier, q.Official.Tier)
		if q.Official.OpusKnown {
			combined.Official.OpusKnown = true
			combined.Official.OpusPercent += q.projectedOpusPercent()
			combined.Official.OpusNegative = combined.Official.OpusNegative && q.Official.OpusNegative
			if len(combined.Official.Usage) == 0 {
				combined.Official.Usage = q.Official.Usage
			}
			if !q.OpusNextPercentAt.IsZero() && (combined.OpusNextPercentAt.IsZero() || q.OpusNextPercentAt.Before(combined.OpusNextPercentAt)) {
				combined.OpusNextPercentAt = q.OpusNextPercentAt
			}
			combined.OpusPredicted = combined.OpusPredicted || q.OpusPredicted
		}
		if combined.Refreshed.IsZero() || q.Refreshed.Before(combined.Refreshed) {
			combined.Refreshed = q.Refreshed
		}
	}
	if combined.Refreshed.IsZero() {
		if combined.UnknownBalance {
			combined.Refreshed = time.Now()
			combined.Official.Active = true
			combined.Official.Tier = 3
			return combined, nil
		}
		return nil, firstErr
	}
	combined.Official.OpusPercent = min(100, combined.Official.OpusPercent)
	return combined, nil
}

func (h *ManagedHandler) fetchQuota(ctx context.Context, token string) (upstreamQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.imageURL.String()+"/user/subscription", nil)
	if err != nil {
		return upstreamQuota{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
		Percent     *float64 `json:"percent"`
		Negative    bool     `json:"isNegative"`
		NextPercent *float64 `json:"timeUntilNextPercent"`
	}
	if len(data.Usage) > 0 && json.Unmarshal(data.Usage, &usage) == nil && usage.Percent != nil && !math.IsNaN(*usage.Percent) && !math.IsInf(*usage.Percent, 0) && *usage.Percent >= 0 && *usage.Percent <= 100 {
		quota.OpusPercent = *usage.Percent
		quota.OpusKnown = true
		quota.OpusNegative = usage.Negative
		if usage.NextPercent != nil && !math.IsNaN(*usage.NextPercent) && !math.IsInf(*usage.NextPercent, 0) && *usage.NextPercent > 0 && *usage.NextPercent <= 7*24*3600 {
			quota.NextPercentSeconds = int64(math.Ceil(*usage.NextPercent))
		}
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
			view := h.viewKey(latest)
			view.QueueLength = h.queueLength()
			view.KeyQueueLength = h.keyQueueLength(key.ID)
			jsonReply(w, http.StatusOK, view)
			return
		}
	}
	http.Error(w, "key unavailable", http.StatusUnauthorized)
}

func (h *ManagedHandler) serveSubscription(w http.ResponseWriter, r *http.Request, key clientKey) {
	release, err := h.enter(r.Context(), key.ID, r.Method+" "+r.URL.Path, keyQueueLimit(key))
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
	q, err := h.quotaForKey(r.Context(), current)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	for _, latest := range h.store.snapshot() {
		if latest.ID == key.ID && latest.Hash == current.Hash && !latest.Revoked {
			fixed, purchased := min(fixedRemaining(latest), q.Fixed), min(purchasedRemaining(latest), q.Purchased)
			balanceSource := "upstream_projection"
			if q.UnknownBalance {
				fixed = min(fixedRemaining(latest), 10000)
				purchased = min(purchasedRemaining(latest), 10000-fixed)
				balanceSource = "local_budget_upstream_unknown"
			}
			jsonReply(w, http.StatusOK, map[string]any{
				"active": q.Official.Active, "isGracePeriod": q.Official.Grace,
				"tier": q.Official.Tier,
				"trainingStepsLeft": map[string]int64{
					"fixedTrainingStepsLeft": fixed,
					"purchasedTrainingSteps": purchased,
				},
				"usage":          projectedUsage(q, h.opusRemainingForSubscription(latest)),
				"balance_source": balanceSource,
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
	if errors.Is(err, errQueueDraining) {
		http.Error(w, "service restarting", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Retry-After", "2")
	http.Error(w, err.Error(), http.StatusTooManyRequests)
}

func (h *ManagedHandler) serveJob(w http.ResponseWriter, r *http.Request, key clientKey, selected *route) {
	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	release, err := h.enter(r.Context(), key.ID, r.Method+" "+r.URL.Path, keyQueueLimit(key))
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	h.executeJob(w, r, key, selected)
}

func (h *ManagedHandler) executeJob(w http.ResponseWriter, r *http.Request, key clientKey, selected *route) {
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
	accounts := h.accountCandidatesForJob(current, selected.path, cost.Model)
	if len(accounts) == 0 {
		http.Error(w, "no enabled upstream account supports this route and model", http.StatusServiceUnavailable)
		return
	}
	start := 0
	if keyAccountID(current) == poolAccountID {
		start = h.poolCursor % len(accounts)
	}
	var q *quotaSnapshot
	var hold reservation
	var selectedAccount upstreamAccount
	var quotaErr, reservationErr error
	for offset := range accounts {
		index := (start + offset) % len(accounts)
		account := accounts[index]
		candidate, err := h.currentQuota(r.Context(), account.ID, false)
		if err != nil {
			quotaErr = err
			continue
		}
		for _, latest := range h.store.snapshot() {
			if latest.ID == current.ID {
				current = latest
				break
			}
		}
		candidateHold, err := chooseReservation(current, cost, candidate)
		if err != nil {
			reservationErr = err
			continue
		}
		if candidateHold.Opus > 0 {
			states := h.store.opusSnapshot()
			state, known := states[account.ID]
			if opusMode(current) == "percent" && (!candidate.Official.OpusKnown || !known || h.opusAvailable(current, account.ID) < candidateHold.Opus) {
				reservationErr = errors.New("Opus quota unavailable for key on account")
				continue
			}
			if known && opusMode(current) == "images" && state.Projected-opusAllocated(h.store.snapshot(), account.ID) < candidateHold.Opus*opusUnit {
				reservationErr = errors.New("unallocated Opus quota unavailable")
				continue
			}
		}
		selectedAccount, q, hold = account, candidate, candidateHold
		if keyAccountID(current) == poolAccountID {
			h.poolCursor = (index + 1) % len(accounts)
		}
		break
	}
	if q == nil {
		if quotaErr != nil {
			http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		} else if reservationErr != nil {
			http.Error(w, reservationErr.Error(), http.StatusPaymentRequired)
		} else {
			http.Error(w, "upstream account unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	token, err := h.accountToken(selectedAccount.ID)
	if err != nil {
		http.Error(w, "upstream account unavailable", http.StatusBadGateway)
		return
	}
	imageProxy := h.image
	if selectedAccount.provider() == providerNewAPI {
		origin, err := parseUpstream(selectedAccount.Origin)
		if err != nil {
			http.Error(w, "upstream origin unavailable", http.StatusBadGateway)
			return
		}
		imageProxy = newReverseProxy(origin, "/image", h.transport)
	}
	var projectedOpus *float64
	err = h.store.updateWithOpus(func(keys []clientKey, states map[string]opusAccountState) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == key.ID && keys[i].Hash == currentKey.Hash && !keys[i].Revoked && (!cost.MultiImage || (h.settings.snapshot().AllowMultiImage && keys[i].AllowMultiImage)) && fixedRemaining(keys[i]) >= hold.Fixed && purchasedRemaining(keys[i]) >= hold.Purchased && opusRemaining(keys[i]) >= hold.Opus {
				if hold.Opus > 0 {
					state, known := states[selectedAccount.ID]
					if opusMode(keys[i]) == "percent" {
						bucket := bucketFor(&keys[i], selectedAccount.ID)
						if !known || !bucket.Seeded || bucket.Balance < hold.Opus*opusUnit {
							return nil, errors.New("Opus quota unavailable for key on account")
						}
						bucket.Balance -= hold.Opus * opusUnit
						bucket.Pending += hold.Opus
						keys[i].OpusBuckets[selectedAccount.ID] = bucket
					} else if known {
						if state.Projected-opusAllocated(keys, selectedAccount.ID) < hold.Opus*opusUnit {
							return nil, errors.New("unallocated Opus quota unavailable")
						}
						bucket := bucketFor(&keys[i], selectedAccount.ID)
						bucket.Pending += hold.Opus
						keys[i].OpusBuckets[selectedAccount.ID] = bucket
					}
					if known {
						if state.Projected < hold.Opus*opusUnit {
							return nil, errors.New("upstream Opus quota unavailable")
						}
						state.Projected -= hold.Opus * opusUnit
						states[selectedAccount.ID] = state
						value := float64(state.Projected) * 100 / float64(opusFullUnits)
						projectedOpus = &value
					}
				}
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
	if !q.UnknownBalance {
		q.Fixed -= hold.Fixed
		q.Purchased -= hold.Purchased
	}
	if projectedOpus != nil {
		q.OpusProjected = projectedOpus
	} else if cost.V5 {
		q.OpusJobsSinceRefresh += hold.Opus
	}
	upstreamRequest := r.Clone(r.Context())
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Authorization", "Bearer "+token)
	upstreamRequest.Body = io.NopCloser(bytes.NewReader(body))
	upstreamRequest.ContentLength = int64(len(body))
	tracked := &statusWriter{ResponseWriter: w}
	if strings.HasSuffix(selected.path, "/ai/generate-image-stream") {
		tracked.stream = &streamOutcome{}
	}
	var capture *archiveWriter
	archiveEnabled := (strings.HasSuffix(selected.path, "/ai/generate-image") || strings.HasSuffix(selected.path, "/ai/generate-image-stream")) && h.settings.snapshot().ArchiveEnabled && !current.ArchiveDisabled
	if archiveEnabled {
		capture, err = h.archive.capture(tracked)
		if err != nil {
			h.archive.recordFailure(err)
		}
	}
	if capture != nil {
		defer func() { _ = capture.file.Close(); _ = os.Remove(capture.file.Name()) }()
		imageProxy.ServeHTTP(capture, upstreamRequest)
	} else {
		imageProxy.ServeHTTP(tracked, upstreamRequest)
	}
	if capture != nil {
		id, idErr := randomHex(16)
		if idErr != nil {
			h.archive.recordFailure(idErr)
		} else {
			groupID := id
			if durableID, ok := r.Context().Value(archiveJobIDKey{}).(string); ok {
				groupID = durableID
			}
			h.archive.finishCapture(capture, archiveWork{ID: id, GroupID: groupID, KeyID: current.ID, KeyName: current.Name, IP: h.clientIP(r), Route: selected.path, Completed: time.Now()}, tracked.status >= 200 && tracked.status < 300 && r.Context().Err() == nil)
		}
	}
	if tracked.status < 200 || (tracked.status >= 300 && tracked.status < 400) || tracked.status >= 500 || r.Context().Err() != nil {
		return // The upstream outcome is uncertain; keep the reservation pending.
	}
	refund := tracked.status >= 400
	generated := !refund && cost.Samples > 0 && tracked.bodyBytes > 0 && tracked.generatedImage()
	projectedOpus = nil
	err = h.store.updateWithOpus(func(keys []clientKey, states map[string]opusAccountState) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == key.ID {
				if hold.Opus > 0 {
					bucket, ok := keys[i].OpusBuckets[selectedAccount.ID]
					if ok {
						bucket.Pending = max(0, bucket.Pending-hold.Opus)
						if refund && opusMode(keys[i]) == "percent" {
							bucket.Balance = min(opusCapacity(keys, keys[i], selectedAccount.ID), bucket.Balance+hold.Opus*opusUnit)
						}
						keys[i].OpusBuckets[selectedAccount.ID] = bucket
					}
					if refund {
						if state, ok := states[selectedAccount.ID]; ok {
							state.Projected = min(opusFullUnits, state.Projected+hold.Opus*opusUnit)
							states[selectedAccount.ID] = state
							value := float64(state.Projected) * 100 / float64(opusFullUnits)
							projectedOpus = &value
						}
					}
				}
				keys[i].FixedPending -= hold.Fixed
				keys[i].PurchasedPending -= hold.Purchased
				keys[i].OpusPending -= hold.Opus
				if refund {
					keys[i].FixedSpent -= hold.Fixed
					keys[i].PurchasedSpent -= hold.Purchased
					keys[i].OpusUsed -= hold.Opus
				}
				if generated {
					keys[i].SuccessfulGenerations++
					keys[i].SuccessfulImages += int64(cost.Samples)
					keys[i].FormulaAnlas += cost.FormulaAnlas
				}
				break
			}
		}
		return keys, nil
	})
	if err == nil && refund {
		if !q.UnknownBalance {
			q.Fixed += hold.Fixed
			q.Purchased += hold.Purchased
		}
		if projectedOpus != nil {
			q.OpusProjected = projectedOpus
		} else if cost.V5 {
			q.OpusJobsSinceRefresh -= hold.Opus
		}
	}
	if err == nil && generated {
		if saveErr := h.db.recordGeneration(time.Now(), cost.Samples); saveErr != nil {
			slog.Warn("generation activity unavailable", "error", saveErr)
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
	if q.OpusPredicted {
		delete(usage, "timeUntilNextPercent")
	} else if !q.OpusNextPercentAt.IsZero() {
		remaining := max(0, int64(math.Ceil(time.Until(q.OpusNextPercentAt).Seconds())))
		countdown, _ := json.Marshal(remaining)
		usage["timeUntilNextPercent"] = countdown
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
