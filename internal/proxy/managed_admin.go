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
	AllocatedAnlas     int64   `json:"allocated_anlas"`
	SpentAnlas         int64   `json:"spent_anlas"`
	PendingAnlas       int64   `json:"pending_anlas"`
	RemainingAnlas     int64   `json:"remaining_anlas"`
	QueueLength        int     `json:"queue_length,omitempty"`
	Revoked            bool    `json:"revoked"`
	Key                string  `json:"key,omitempty"`
}

func viewKey(k clientKey) publicKey {
	return publicKey{
		ID: k.ID, Name: k.Name,
		AllowFixed: k.AllowFixed, FixedLimit: k.FixedLimit, FixedSpent: k.FixedSpent,
		FixedPending: k.FixedPending, FixedRemaining: displayRemaining(fixedRemaining(k)),
		AllowPurchased: k.AllowPurchased, PurchasedLimit: k.PurchasedLimit,
		PurchasedSpent: k.PurchasedSpent, PurchasedPending: k.PurchasedPending,
		PurchasedRemaining: displayRemaining(purchasedRemaining(k)),
		AllowOpus:          k.AllowOpus, AllowMultiImage: k.AllowMultiImage, OpusLimit: k.OpusLimit, OpusUsed: k.OpusUsed,
		OpusLimitMode: opusMode(k), OpusLimitPercent: k.OpusLimitPercent, OpusEffectiveLimit: opusEffectiveLimit(k),
		OpusPending: k.OpusPending, OpusRemaining: displayRemaining(opusRemaining(k)),
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
	release, err := h.enter(r.Context())
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	q, err := h.currentQuota(r.Context(), force)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	keys := h.store.snapshot()
	fixedAllocated, purchasedAllocated := totalRemaining(keys)
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
		"unlimited_fixed_keys": unlimitedFixed, "unlimited_purchased_keys": unlimitedPurchased, "unlimited_opus_keys": unlimitedOpus,
		"allocated_remaining_anlas":   fixedAllocated + purchasedAllocated,
		"unallocated_fixed_anlas":     max(0, q.Fixed-fixedAllocated),
		"unallocated_purchased_anlas": max(0, q.Purchased-purchasedAllocated),
		"unallocated_anlas":           max(0, q.Fixed-fixedAllocated) + max(0, q.Purchased-purchasedAllocated),
		"active":                      q.Official.Active, "isGracePeriod": q.Official.Grace, "tier": q.Official.Tier,
		"usage": projectedUsage(q, -1), "projected_opus_percent": q.projectedOpusPercent(),
		"snapshot_age_seconds": int64(time.Since(q.Refreshed).Seconds()),
		"queue_length":         len(h.queue),
	})
}

func (h *ManagedHandler) createKey(w http.ResponseWriter, r *http.Request, input keyPolicyInput) {
	if input.AllowMultiImage != nil && *input.AllowMultiImage && !h.settings.snapshot().AllowMultiImage {
		http.Error(w, "enable multi-image in settings first", http.StatusConflict)
		return
	}
	key := clientKey{Name: input.Name, PolicyVersion: 1}
	if err := applyPolicy(&key, input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	release, err := h.enter(r.Context())
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	q, err := h.currentQuota(r.Context(), false)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
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
		fixed, purchased := totalRemaining(keys)
		if (key.FixedLimit != -1 && fixed+fixedRemaining(key) > q.Fixed) || (key.PurchasedLimit != -1 && purchased+purchasedRemaining(key) > q.Purchased) {
			return nil, errors.New("not enough unallocated upstream Anlas")
		}
		return append(keys, key), nil
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
	release, err := h.enter(r.Context())
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
	release, err := h.enter(r.Context())
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	q, err := h.currentQuota(r.Context(), false)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	var updated clientKey
	err = h.store.update(func(keys []clientKey) ([]clientKey, error) {
		for i := range keys {
			if keys[i].ID == id && !keys[i].Revoked {
				wasAllowed := keys[i].AllowMultiImage
				if err := applyPolicy(&keys[i], input); err != nil {
					return nil, err
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
				fixed, purchased := totalRemaining(keys)
				if fixed > q.Fixed || purchased > q.Purchased {
					return nil, errors.New("not enough unallocated upstream Anlas")
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
	release, err := h.enter(r.Context())
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
	h.quota = nil
	h.lastQuotaAttempt = time.Time{}
	jsonReply(w, http.StatusOK, viewKey(updated))
}
