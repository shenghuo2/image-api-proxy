package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type publicKey struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	AccountID          string  `json:"account_id"`
	AccountName        string  `json:"account_name,omitempty"`
	AllowFixed         bool    `json:"allow_fixed_anlas"`
	FixedLimit         int64   `json:"fixed_anlas_limit"`
	FixedSpent         int64   `json:"fixed_anlas_spent"`
	FixedPending       int64   `json:"fixed_anlas_pending"`
	FixedRemaining     int64   `json:"fixed_anlas_remaining"`
	AllowPurchased     bool    `json:"allow_purchased_anlas"`
	PurchasedLimit     int64   `json:"purchased_anlas_limit"`
	PurchasedSpent     int64   `json:"purchased_anlas_spent"`
	PurchasedPending   int64   `json:"purchased_anlas_pending"`
	PurchasedRemaining int64   `json:"purchased_anlas_remaining"`
	AllowOpus          bool    `json:"allow_opus"`
	AllowMultiImage    bool    `json:"allow_multi_image"`
	OpusLimitMode      string  `json:"opus_limit_mode"`
	OpusLimitPercent   float64 `json:"opus_limit_percent"`
	OpusLimit          int64   `json:"opus_limit_images"`
	OpusEffectiveLimit int64   `json:"opus_effective_limit_images"`
	OpusUsed           int64   `json:"opus_used_images"`
	OpusPending        int64   `json:"opus_pending_images"`
	OpusRemaining      int64   `json:"opus_remaining_images"`
	QueueLimit         int     `json:"queue_limit"`
	AllocatedAnlas     int64   `json:"allocated_anlas"`
	SpentAnlas         int64   `json:"spent_anlas"`
	PendingAnlas       int64   `json:"pending_anlas"`
	RemainingAnlas     int64   `json:"remaining_anlas"`
	QueueLength        int     `json:"queue_length"`
	KeyQueueLength     int     `json:"key_queue_length"`
	Revoked            bool    `json:"revoked"`
	Key                string  `json:"key,omitempty"`
}

func viewKey(k clientKey) publicKey {
	return publicKey{
		ID: k.ID, Name: k.Name, AccountID: keyAccountID(k),
		AllowFixed: k.AllowFixed, FixedLimit: k.FixedLimit, FixedSpent: k.FixedSpent,
		FixedPending: k.FixedPending, FixedRemaining: displayRemaining(fixedRemaining(k)),
		AllowPurchased: k.AllowPurchased, PurchasedLimit: k.PurchasedLimit,
		PurchasedSpent: k.PurchasedSpent, PurchasedPending: k.PurchasedPending,
		PurchasedRemaining: displayRemaining(purchasedRemaining(k)),
		AllowOpus:          k.AllowOpus, AllowMultiImage: k.AllowMultiImage, OpusLimit: k.OpusLimit, OpusUsed: k.OpusUsed,
		OpusLimitMode: opusMode(k), OpusLimitPercent: k.OpusLimitPercent, OpusEffectiveLimit: opusEffectiveLimit(k),
		OpusPending: k.OpusPending, OpusRemaining: displayRemaining(opusRemaining(k)),
		QueueLimit:     keyQueueLimit(k),
		AllocatedAnlas: allocatedAnlas(k),
		SpentAnlas:     k.FixedSpent + k.PurchasedSpent,
		PendingAnlas:   k.FixedPending + k.PurchasedPending,
		RemainingAnlas: remaining(k), Revoked: k.Revoked,
	}
}

func opusMode(k clientKey) string {
	if k.OpusLimitMode == "percent" {
		return "percent"
	}
	return "images"
}

func (h *ManagedHandler) viewAdminKey(k clientKey) publicKey {
	view := viewKey(k)
	if view.AccountID == poolAccountID {
		view.AccountName = "账号池"
	} else if account, ok := h.accounts.find(view.AccountID); ok {
		view.AccountName = account.Name
	}
	if k.KeyCiphertext != "" {
		if raw, err := h.vault.open(k.KeyCiphertext); err == nil && hexHash(raw) == k.Hash {
			view.Key = raw
		}
	}
	return view
}

func allocatedAnlas(k clientKey) int64 {
	if (k.AllowFixed && k.FixedLimit == -1) || (k.AllowPurchased && k.PurchasedLimit == -1) {
		return -1
	}
	return max(0, k.FixedLimit) + max(0, k.PurchasedLimit)
}

func decodeAdminBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8193))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected a single JSON object")
	}
	return nil
}

func (h *ManagedHandler) serveAdmin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/admin/accounts" || strings.HasPrefix(r.URL.Path, "/admin/accounts/"):
		h.serveAdminAccounts(w, r)
	case r.URL.Path == "/admin/settings" && r.Method == http.MethodGet:
		jsonReply(w, http.StatusOK, h.settings.snapshot())
	case r.URL.Path == "/admin/settings" && r.Method == http.MethodPut:
		var input struct {
			AllowMultiImage *bool `json:"allow_multi_image"`
		}
		if err := decodeAdminBody(r, &input); err != nil || input.AllowMultiImage == nil {
			http.Error(w, "invalid settings", http.StatusBadRequest)
			return
		}
		if err := h.settings.set(proxySettings{AllowMultiImage: *input.AllowMultiImage}); err != nil {
			http.Error(w, "settings unavailable", http.StatusInternalServerError)
			return
		}
		jsonReply(w, http.StatusOK, h.settings.snapshot())
	case r.URL.Path == "/admin/quota" && r.Method == http.MethodGet:
		h.serveAdminQuota(w, r, false)
	case r.URL.Path == "/admin/queue" && r.Method == http.MethodGet:
		h.serveAdminQueue(w, r)
	case r.URL.Path == "/admin/quota/refresh" && r.Method == http.MethodPost:
		h.serveAdminQuota(w, r, true)
	case r.URL.Path == "/admin/keys" && r.Method == http.MethodGet:
		keys := h.store.snapshot()
		out := make([]publicKey, 0, len(keys))
		for _, key := range keys {
			out = append(out, h.viewAdminKey(key))
		}
		jsonReply(w, http.StatusOK, out)
	case r.URL.Path == "/admin/keys" && r.Method == http.MethodPost:
		var input keyPolicyInput
		if err := decodeAdminBody(r, &input); err != nil || len(input.Name) == 0 || len(input.Name) > 80 {
			http.Error(w, "invalid key request", http.StatusBadRequest)
			return
		}
		h.createKey(w, r, input)
	case strings.HasPrefix(r.URL.Path, "/admin/keys/") && r.Method == http.MethodPut:
		var input keyPolicyInput
		if err := decodeAdminBody(r, &input); err != nil {
			http.Error(w, "invalid policy", http.StatusBadRequest)
			return
		}
		h.setPolicy(w, r, strings.TrimPrefix(r.URL.Path, "/admin/keys/"), input)
	case strings.HasPrefix(r.URL.Path, "/admin/keys/") && strings.HasSuffix(r.URL.Path, "/reconcile") && r.Method == http.MethodPost:
		var body struct {
			Charged     *int64 `json:"charged_anlas"`
			OpusCharged *int64 `json:"opus_charged_images"`
		}
		if err := decodeAdminBody(r, &body); err != nil || body.Charged == nil || *body.Charged < 0 || *body.Charged > 1e9 || (body.OpusCharged != nil && (*body.OpusCharged < 0 || *body.OpusCharged > 1e7)) {
			http.Error(w, "invalid reconciliation", http.StatusBadRequest)
			return
		}
		h.reconcile(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/keys/"), "/reconcile"), *body.Charged, body.OpusCharged)
	case strings.HasPrefix(r.URL.Path, "/admin/keys/") && strings.HasSuffix(r.URL.Path, "/rotate") && r.Method == http.MethodPost:
		h.rotateKey(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/keys/"), "/rotate"))
	case strings.HasPrefix(r.URL.Path, "/admin/keys/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
		err := h.store.update(func(keys []clientKey) ([]clientKey, error) {
			for i := range keys {
				if keys[i].ID == id && !keys[i].Revoked {
					keys[i].Revoked = true
					return keys, nil
				}
			}
			return nil, errors.New("key not found")
		})
		if err != nil {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (h *ManagedHandler) serveAdminQuota(w http.ResponseWriter, r *http.Request, force bool) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	keys := h.store.snapshot()
	accounts := h.accounts.snapshot()
	q := &quotaSnapshot{}
	accountQuotas := make([]map[string]any, 0, len(accounts))
	accountErrors := make([]map[string]string, 0)
	var fixedAllocated, purchasedAllocated int64
	for _, account := range accounts {
		if account.Disabled {
			continue
		}
		current, err := h.currentQuota(r.Context(), account.ID, force)
		if err != nil {
			accountErrors = append(accountErrors, map[string]string{"account_id": account.ID, "name": account.Name})
			continue
		}
		accountQuotas = append(accountQuotas, accountQuotaView(account, current, keys))
		fixed, purchased := totalRemainingForAccount(keys, account.ID)
		fixedAllocated += fixed
		purchasedAllocated += purchased
		q.Fixed += current.Fixed
		q.Purchased += current.Purchased
		q.Official.Fixed += current.Official.Fixed
		q.Official.Purchased += current.Official.Purchased
		q.Official.Active = q.Official.Active || current.Official.Active
		q.Official.Grace = q.Official.Grace || current.Official.Grace
		q.Official.Tier = max(q.Official.Tier, current.Official.Tier)
		if current.Official.OpusKnown {
			q.Official.OpusKnown = true
			q.Official.OpusPercent += current.projectedOpusPercent()
			if len(q.Official.Usage) == 0 {
				q.Official.Usage = current.Official.Usage
			}
		}
		if q.Refreshed.IsZero() || current.Refreshed.Before(q.Refreshed) {
			q.Refreshed = current.Refreshed
		}
	}
	if len(accountQuotas) == 0 {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	poolFixed, poolPurchased := totalRemainingForPool(keys)
	fixedAllocated += poolFixed
	purchasedAllocated += poolPurchased
	q.Official.OpusPercent = min(100, q.Official.OpusPercent)
	var unlimitedFixed, unlimitedPurchased, unlimitedOpus int
	for _, key := range keys {
		if key.Revoked {
			continue
		}
		if key.AllowFixed && key.FixedLimit == -1 {
			unlimitedFixed++
		}
		if key.AllowPurchased && key.PurchasedLimit == -1 {
			unlimitedPurchased++
		}
		if key.AllowOpus && opusEffectiveLimit(key) == -1 {
			unlimitedOpus++
		}
	}
	jsonReply(w, http.StatusOK, map[string]any{
		"upstream_fixed_anlas": q.Official.Fixed, "upstream_purchased_anlas": q.Official.Purchased,
		"upstream_anlas":        q.Official.Fixed + q.Official.Purchased,
		"projected_fixed_anlas": q.Fixed, "projected_purchased_anlas": q.Purchased,
		"allocated_fixed_anlas": fixedAllocated, "allocated_purchased_anlas": purchasedAllocated,
		"account_quotas": accountQuotas, "account_errors": accountErrors,
		"unlimited_fixed_keys": unlimitedFixed, "unlimited_purchased_keys": unlimitedPurchased, "unlimited_opus_keys": unlimitedOpus,
		"allocated_remaining_anlas":   fixedAllocated + purchasedAllocated,
		"unallocated_fixed_anlas":     max(0, q.Fixed-fixedAllocated),
		"unallocated_purchased_anlas": max(0, q.Purchased-purchasedAllocated),
		"unallocated_anlas":           max(0, q.Fixed-fixedAllocated) + max(0, q.Purchased-purchasedAllocated),
		"active":                      q.Official.Active, "isGracePeriod": q.Official.Grace, "tier": q.Official.Tier,
		"usage": projectedUsage(q, -1), "projected_opus_percent": q.projectedOpusPercent(),
		"snapshot_age_seconds": int64(time.Since(q.Refreshed).Seconds()),
		"queue_length":         h.queueLength(),
	})
}

func (h *ManagedHandler) createKey(w http.ResponseWriter, r *http.Request, input keyPolicyInput) {
	if input.AllowMultiImage != nil && *input.AllowMultiImage && !h.settings.snapshot().AllowMultiImage {
		http.Error(w, "enable multi-image in settings first", http.StatusConflict)
		return
	}
	key := clientKey{Name: input.Name, AccountID: poolAccountID, PolicyVersion: 1}
	if input.AccountID != nil {
		key.AccountID = *input.AccountID
	}
	if !h.validAccountChoice(key.AccountID) {
		http.Error(w, "account unavailable", http.StatusBadRequest)
		return
	}
	if err := applyPolicy(&key, input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	if !h.validAccountChoice(key.AccountID) {
		http.Error(w, "account unavailable", http.StatusConflict)
		return
	}
	quotas, err := h.activeQuotas(r.Context())
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	if key.AccountID != poolAccountID && quotas[key.AccountID] == nil {
		http.Error(w, "upstream account quota unavailable", http.StatusBadGateway)
		return
	}
	key.ID, err = randomHex(8)
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	raw, err := randomClientKey()
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	key.Hash = hexHash(raw)
	key.KeyCiphertext, err = h.vault.seal(raw)
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		if key.AllowMultiImage && !h.settings.snapshot().AllowMultiImage {
			return nil, errors.New("enable multi-image in settings first")
		}
		keys = append(keys, key)
		if err := validateAccountAllocations(keys, quotas); err != nil {
			return nil, err
		}
		return keys, nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, http.StatusCreated, map[string]any{"key": raw, "client": viewKey(key)})
}

func (h *ManagedHandler) rotateKey(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	raw, err := randomClientKey()
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	ciphertext, err := h.vault.seal(raw)
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	var updated clientKey
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == id && !keys[i].Revoked {
				keys[i].Hash = hexHash(raw)
				keys[i].KeyCiphertext = ciphertext
				updated = keys[i]
				return keys, nil
			}
		}
		return nil, errors.New("key not found")
	})
	if err != nil {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"key": raw, "client": viewKey(updated)})
}

func (h *ManagedHandler) setPolicy(w http.ResponseWriter, r *http.Request, id string, input keyPolicyInput) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	quotas, err := h.activeQuotas(r.Context())
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	var updated clientKey
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == id && !keys[i].Revoked {
				if input.AccountID != nil && *input.AccountID != keyAccountID(keys[i]) {
					if !h.validAccountChoice(*input.AccountID) {
						return nil, errors.New("account unavailable")
					}
					if keys[i].FixedSpent+keys[i].PurchasedSpent+keys[i].OpusUsed != 0 {
						return nil, errors.New("cannot move a key with usage to another account")
					}
					keys[i].AccountID = *input.AccountID
				}
				wasAllowed := keys[i].AllowMultiImage
				if err := applyPolicy(&keys[i], input); err != nil {
					return nil, err
				}
				if keyAccountID(keys[i]) != poolAccountID && quotas[keyAccountID(keys[i])] == nil {
					return nil, errors.New("upstream account quota unavailable")
				}
				if keys[i].AllowMultiImage && !wasAllowed && !h.settings.snapshot().AllowMultiImage {
					return nil, errors.New("enable multi-image in settings first")
				}
				if input.Name != "" {
					if len(input.Name) > 80 {
						return nil, errors.New("invalid name")
					}
					keys[i].Name = input.Name
				}
				if err := validateAccountAllocations(keys, quotas); err != nil {
					return nil, err
				}
				updated = keys[i]
				return keys, nil
			}
		}
		return nil, errors.New("key not found")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, http.StatusOK, viewKey(updated))
}

func (h *ManagedHandler) reconcile(w http.ResponseWriter, r *http.Request, id string, charged int64, opusCharged *int64) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	var updated clientKey
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			k := &keys[i]
			if k.ID != id || k.FixedPending+k.PurchasedPending+k.OpusPending == 0 {
				continue
			}
			fixedCharge := min(charged, k.FixedPending)
			purchasedCharge := charged - fixedCharge
			if k.PurchasedPending == 0 {
				fixedCharge += purchasedCharge
				purchasedCharge = 0
			}
			k.FixedSpent = max(0, k.FixedSpent+fixedCharge-k.FixedPending)
			k.PurchasedSpent = max(0, k.PurchasedSpent+purchasedCharge-k.PurchasedPending)
			k.FixedPending, k.PurchasedPending = 0, 0
			opus := k.OpusPending
			if opusCharged != nil {
				opus = *opusCharged
			}
			k.OpusUsed = max(0, k.OpusUsed+opus-k.OpusPending)
			k.OpusPending = 0
			updated = *k
			return keys, nil
		}
		return nil, errors.New("no pending reservation")
	})
	if err != nil {
		http.Error(w, "no pending reservation", http.StatusConflict)
		return
	}
	// A previous refresh may already include the actual upstream charge.
	// Re-fetch after manual reconciliation instead of applying its delta twice.
	h.quotas = make(map[string]*quotaSnapshot)
	h.lastQuotaAttempt = make(map[string]time.Time)
	jsonReply(w, http.StatusOK, viewKey(updated))
}
