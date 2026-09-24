package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type publicAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	TokenConfigured bool   `json:"token_configured"`
	KeyCount        int    `json:"key_count"`
}

type accountInput struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Enabled *bool  `json:"enabled"`
}

func (h *ManagedHandler) viewAccount(account upstreamAccount, keys []clientKey) publicAccount {
	_, err := h.vault.open(account.TokenCiphertext)
	view := publicAccount{ID: account.ID, Name: account.Name, Enabled: !account.Disabled, TokenConfigured: err == nil}
	for _, key := range keys {
		if keyAccountID(key) == account.ID {
			view.KeyCount++
		}
	}
	return view
}

func (h *ManagedHandler) validAccountChoice(id string) bool {
	if id == poolAccountID {
		for _, account := range h.accounts.snapshot() {
			if !account.Disabled {
				return true
			}
		}
		return false
	}
	account, ok := h.accounts.find(id)
	return ok && !account.Disabled
}

func (h *ManagedHandler) duplicateAccountToken(token, exceptID string) bool {
	for _, account := range h.accounts.snapshot() {
		if account.ID == exceptID {
			continue
		}
		if existing, err := h.vault.open(account.TokenCiphertext); err == nil && existing == token {
			return true
		}
	}
	return false
}

func (h *ManagedHandler) activeQuotas(ctx context.Context) (map[string]*quotaSnapshot, error) {
	quotas := make(map[string]*quotaSnapshot)
	var firstErr error
	for _, account := range h.accounts.snapshot() {
		if account.Disabled {
			continue
		}
		q, err := h.currentQuota(ctx, account.ID, false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		quotas[account.ID] = q
	}
	if len(quotas) == 0 {
		if firstErr == nil {
			firstErr = errors.New("no enabled upstream account")
		}
		return nil, firstErr
	}
	return quotas, nil
}

func validateAccountAllocations(keys []clientKey, quotas map[string]*quotaSnapshot) error {
	var freeFixed, freePurchased int64
	for id, q := range quotas {
		fixed, purchased := totalRemainingForAccount(keys, id)
		if fixed > q.Fixed || purchased > q.Purchased {
			return errors.New("not enough unallocated upstream Anlas")
		}
		freeFixed += q.Fixed - fixed
		freePurchased += q.Purchased - purchased
	}
	poolFixed, poolPurchased := totalRemainingForPool(keys)
	if poolFixed > freeFixed || poolPurchased > freePurchased {
		return errors.New("not enough unallocated upstream Anlas")
	}
	return nil
}

func (h *ManagedHandler) serveAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/accounts" {
		switch r.Method {
		case http.MethodGet:
			keys := h.store.snapshot()
			views := make([]publicAccount, 0)
			for _, account := range h.accounts.snapshot() {
				views = append(views, h.viewAccount(account, keys))
			}
			jsonReply(w, http.StatusOK, views)
		case http.MethodPost:
			h.createAccount(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		switch r.Method {
		case http.MethodPut:
			h.updateAccount(w, r, parts[0])
		case http.MethodDelete:
			h.deleteAccount(w, r, parts[0])
		default:
			http.NotFound(w, r)
		}
		return
	}
	if (len(parts) == 2 && parts[1] == "quota" && r.Method == http.MethodGet) ||
		(len(parts) == 3 && parts[1] == "quota" && parts[2] == "refresh" && r.Method == http.MethodPost) {
		h.serveAccountQuota(w, r, parts[0], len(parts) == 3)
		return
	}
	http.NotFound(w, r)
}

func (h *ManagedHandler) createAccount(w http.ResponseWriter, r *http.Request) {
	var input accountInput
	if err := decodeAdminBody(r, &input); err != nil || len(strings.TrimSpace(input.Name)) == 0 || len(input.Name) > 80 || len(input.Token) < 16 || len(input.Token) > 2048 {
		http.Error(w, "invalid account", http.StatusBadRequest)
		return
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	if h.duplicateAccountToken(input.Token, "") {
		http.Error(w, "account token already exists", http.StatusConflict)
		return
	}
	ciphertext, err := h.vault.seal(input.Token)
	if err != nil {
		http.Error(w, "account unavailable", http.StatusInternalServerError)
		return
	}
	accounts := h.accounts.snapshot()
	id := defaultAccountID
	if len(accounts) != 0 {
		id, err = randomHex(8)
		if err != nil {
			http.Error(w, "account unavailable", http.StatusInternalServerError)
			return
		}
	}
	account := upstreamAccount{ID: id, Name: strings.TrimSpace(input.Name), TokenCiphertext: ciphertext}
	if input.Enabled != nil {
		account.Disabled = !*input.Enabled
	}
	if err := h.accounts.update(func(accounts []upstreamAccount) ([]upstreamAccount, error) {
		for _, existing := range accounts {
			if existing.ID == id {
				return nil, errors.New("account ID collision")
			}
		}
		return append(accounts, account), nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, http.StatusCreated, h.viewAccount(account, h.store.snapshot()))
}

func (h *ManagedHandler) updateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var input accountInput
	if err := decodeAdminBody(r, &input); err != nil || len(input.Name) > 80 || len(input.Token) > 2048 || (input.Token != "" && len(input.Token) < 16) {
		http.Error(w, "invalid account", http.StatusBadRequest)
		return
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	if input.Token != "" && h.duplicateAccountToken(input.Token, id) {
		http.Error(w, "account token already exists", http.StatusConflict)
		return
	}
	var ciphertext string
	if input.Token != "" {
		ciphertext, err = h.vault.seal(input.Token)
		if err != nil {
			http.Error(w, "account unavailable", http.StatusInternalServerError)
			return
		}
	}
	var updated upstreamAccount
	err = h.accounts.update(func(accounts []upstreamAccount) ([]upstreamAccount, error) {
		for i := range accounts {
			if accounts[i].ID != id {
				continue
			}
			if name := strings.TrimSpace(input.Name); name != "" {
				accounts[i].Name = name
			}
			if ciphertext != "" {
				accounts[i].TokenCiphertext = ciphertext
			}
			if input.Enabled != nil {
				accounts[i].Disabled = !*input.Enabled
			}
			updated = accounts[i]
			return accounts, nil
		}
		return nil, errors.New("account not found")
	})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	delete(h.quotas, id)
	delete(h.lastQuotaAttempt, id)
	jsonReply(w, http.StatusOK, h.viewAccount(updated, h.store.snapshot()))
}

func (h *ManagedHandler) deleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	if len(h.accounts.snapshot()) == 1 {
		for _, key := range h.store.snapshot() {
			if !key.Revoked && keyAccountID(key) == poolAccountID {
				http.Error(w, "account pool still has keys", http.StatusConflict)
				return
			}
		}
	}
	for _, key := range h.store.snapshot() {
		if keyAccountID(key) == id {
			http.Error(w, "account still has keys", http.StatusConflict)
			return
		}
	}
	err = h.accounts.update(func(accounts []upstreamAccount) ([]upstreamAccount, error) {
		for i, account := range accounts {
			if account.ID == id {
				return append(accounts[:i], accounts[i+1:]...), nil
			}
		}
		return nil, errors.New("account not found")
	})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	delete(h.quotas, id)
	delete(h.lastQuotaAttempt, id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *ManagedHandler) serveAccountQuota(w http.ResponseWriter, r *http.Request, id string, force bool) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	account, ok := h.accounts.find(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q, err := h.currentQuota(r.Context(), id, force)
	if err != nil {
		http.Error(w, "upstream quota unavailable", http.StatusBadGateway)
		return
	}
	jsonReply(w, http.StatusOK, accountQuotaView(account, q, h.store.snapshot()))
}

func accountQuotaView(account upstreamAccount, q *quotaSnapshot, keys []clientKey) map[string]any {
	fixed, purchased := totalRemainingForAccount(keys, account.ID)
	return map[string]any{
		"account_id": account.ID, "name": account.Name,
		"upstream_fixed_anlas": q.Official.Fixed, "upstream_purchased_anlas": q.Official.Purchased,
		"projected_fixed_anlas": q.Fixed, "projected_purchased_anlas": q.Purchased,
		"allocated_fixed_anlas": fixed, "allocated_purchased_anlas": purchased,
		"unallocated_fixed_anlas": max(0, q.Fixed-fixed), "unallocated_purchased_anlas": max(0, q.Purchased-purchased),
		"projected_opus_percent": q.projectedOpusPercent(),
		"active":                 q.Official.Active, "isGracePeriod": q.Official.Grace, "tier": q.Official.Tier,
		"snapshot_age_seconds": int64(time.Since(q.Refreshed).Seconds()),
	}
}
