package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type publicAccount struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Enabled                bool     `json:"enabled"`
	TokenConfigured        bool     `json:"token_configured"`
	KeyCount               int      `json:"key_count"`
	Provider               string   `json:"provider"`
	Origin                 string   `json:"origin,omitempty"`
	EnabledModels          []string `json:"enabled_models,omitempty"`
	FallbackAccountID      string   `json:"fallback_account_id,omitempty"`
	FallbackHighSteps      bool     `json:"fallback_high_steps"`
	FallbackReferenceCount int      `json:"fallback_reference_count"`
}

type accountInput struct {
	Name              string   `json:"name"`
	Token             string   `json:"token"`
	Enabled           *bool    `json:"enabled"`
	Provider          string   `json:"provider"`
	Origin            *string  `json:"origin"`
	EnabledModels     []string `json:"enabled_models"`
	FallbackAccountID *string  `json:"fallback_account_id"`
	FallbackHighSteps *bool    `json:"fallback_high_steps"`
}

func (h *ManagedHandler) viewAccount(account upstreamAccount, keys []clientKey) publicAccount {
	_, err := h.vault.open(account.TokenCiphertext)
	view := publicAccount{ID: account.ID, Name: account.Name, Enabled: !account.Disabled, TokenConfigured: err == nil,
		Provider: account.provider(), Origin: account.Origin, EnabledModels: account.EnabledModels,
		FallbackAccountID: account.FallbackAccountID, FallbackHighSteps: account.FallbackHighSteps}
	for _, source := range h.accounts.snapshot() {
		if source.FallbackAccountID == account.ID {
			view.FallbackReferenceCount++
		}
	}
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

func (h *ManagedHandler) validFallbackAccount(provider, id string) bool {
	if id == "" {
		return true
	}
	account, ok := h.accounts.find(id)
	return provider == providerNewAPI && ok && !account.Disabled && account.provider() == providerNovelAI
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
	var unknownBalance bool
	for id, q := range quotas {
		if q.UnknownBalance {
			unknownBalance = true
			continue
		}
		fixed, purchased := totalRemainingForAccount(keys, id)
		if fixed > q.Fixed || purchased > q.Purchased {
			return errors.New("not enough unallocated upstream Anlas")
		}
		freeFixed += q.Fixed - fixed
		freePurchased += q.Purchased - purchased
	}
	poolFixed, poolPurchased := totalRemainingForPool(keys)
	if !unknownBalance && (poolFixed > freeFixed || poolPurchased > freePurchased) {
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
	if input.Provider == "" {
		input.Provider = providerNovelAI
	}
	if !validAccountConfig(input.Provider, input.Origin, input.EnabledModels) {
		http.Error(w, "invalid account provider, origin or models", http.StatusBadRequest)
		return
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	if input.FallbackAccountID != nil && !h.validFallbackAccount(input.Provider, *input.FallbackAccountID) {
		http.Error(w, "fallback must be an enabled official account", http.StatusBadRequest)
		return
	}
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
	account := upstreamAccount{ID: id, Name: strings.TrimSpace(input.Name), TokenCiphertext: ciphertext, Provider: input.Provider}
	if input.Provider == providerNewAPI {
		account.Origin = *input.Origin
		account.EnabledModels = append([]string(nil), input.EnabledModels...)
	}
	if input.Enabled != nil {
		account.Disabled = !*input.Enabled
	}
	if input.FallbackAccountID != nil {
		account.FallbackAccountID = *input.FallbackAccountID
	}
	if input.FallbackHighSteps != nil {
		account.FallbackHighSteps = *input.FallbackHighSteps
	}
	if account.FallbackHighSteps && (account.FallbackAccountID == "" || !h.validFallbackAccount(account.provider(), account.FallbackAccountID)) {
		http.Error(w, "high-step fallback requires an enabled official fallback account", http.StatusBadRequest)
		return
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
	current, ok := h.accounts.find(id)
	if !ok {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if input.Provider != "" && input.Provider != current.provider() {
		http.Error(w, "account provider cannot be changed", http.StatusBadRequest)
		return
	}
	if input.Origin != nil || input.EnabledModels != nil {
		origin := current.Origin
		models := current.EnabledModels
		if input.Origin != nil {
			origin = *input.Origin
		}
		if input.EnabledModels != nil {
			models = input.EnabledModels
		}
		if !validAccountConfig(current.provider(), &origin, models) {
			http.Error(w, "invalid account origin or models", http.StatusBadRequest)
			return
		}
	}
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	current, ok = h.accounts.find(id)
	if !ok {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	fallbackAccountID, fallbackHighSteps := current.FallbackAccountID, current.FallbackHighSteps
	if input.FallbackAccountID != nil {
		fallbackAccountID = *input.FallbackAccountID
	}
	if input.FallbackHighSteps != nil {
		fallbackHighSteps = *input.FallbackHighSteps
	} else if fallbackAccountID == "" {
		fallbackHighSteps = false
	}
	if fallbackHighSteps && (current.provider() != providerNewAPI || fallbackAccountID == "" ||
		((!current.FallbackHighSteps || fallbackAccountID != current.FallbackAccountID) && !h.validFallbackAccount(current.provider(), fallbackAccountID))) {
		http.Error(w, "high-step fallback requires an enabled official fallback account", http.StatusBadRequest)
		return
	}
	if input.FallbackAccountID != nil && *input.FallbackAccountID != current.FallbackAccountID {
		if !h.validFallbackAccount(current.provider(), *input.FallbackAccountID) {
			http.Error(w, "fallback must be an enabled official account", http.StatusBadRequest)
			return
		}
		for _, key := range h.store.snapshot() {
			if keyAccountID(key) == id && ((!key.Revoked && key.AllowOpus) || key.OpusPending > 0) {
				http.Error(w, "disable Opus on bound keys and reconcile pending Opus before changing fallback", http.StatusConflict)
				return
			}
		}
	}
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
			if input.Origin != nil {
				accounts[i].Origin = *input.Origin
			}
			if input.EnabledModels != nil {
				accounts[i].EnabledModels = append([]string(nil), input.EnabledModels...)
			}
			accounts[i].FallbackAccountID = fallbackAccountID
			accounts[i].FallbackHighSteps = fallbackHighSteps
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

func validAccountConfig(provider string, origin *string, models []string) bool {
	if provider == providerNovelAI {
		return (origin == nil || *origin == "") && len(models) == 0
	}
	if provider != providerNewAPI || origin == nil || !validNewAPIModels(models) {
		return false
	}
	u, err := parseUpstream(*origin)
	return err == nil && u.Fragment == "" && u.String() == *origin
}

func (h *ManagedHandler) deleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	release, err := h.enter(r.Context(), "", r.Method+" "+r.URL.Path, -1)
	if err != nil {
		h.queueError(w, err)
		return
	}
	defer release()
	for _, account := range h.accounts.snapshot() {
		if account.FallbackAccountID == id {
			http.Error(w, "account is still configured as a fallback", http.StatusConflict)
			return
		}
	}
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
	if q.UnknownBalance {
		return map[string]any{
			"account_id": account.ID, "name": account.Name, "provider": account.provider(),
			"upstream_balance_known": false, "upstream_fixed_anlas": nil, "upstream_purchased_anlas": nil,
			"projected_fixed_anlas": nil, "projected_purchased_anlas": nil,
			"allocated_fixed_anlas": fixed, "allocated_purchased_anlas": purchased,
			"unallocated_fixed_anlas": nil, "unallocated_purchased_anlas": nil,
			"projected_opus_percent": nil, "allocated_opus_images": 0, "unallocated_opus_images": nil,
			"opus_predicted": false, "opus_confirmed_at": nil, "opus_next_percent_at": nil,
			"active": true, "isGracePeriod": false, "tier": nil, "snapshot_age_seconds": 0,
		}
	}
	opusAllocatedImages := opusAllocated(keys, account.ID) / opusUnit
	opusAvailableImages := opusUnits(q.projectedOpusPercent()) / opusUnit
	if q.Official.Tier != 3 || (!q.Official.Active && !q.Official.Grace) {
		opusAllocatedImages = 0
	}
	return map[string]any{
		"account_id": account.ID, "name": account.Name, "provider": account.provider(), "upstream_balance_known": true,
		"upstream_fixed_anlas": q.Official.Fixed, "upstream_purchased_anlas": q.Official.Purchased,
		"projected_fixed_anlas": q.Fixed, "projected_purchased_anlas": q.Purchased,
		"allocated_fixed_anlas": fixed, "allocated_purchased_anlas": purchased,
		"unallocated_fixed_anlas": max(0, q.Fixed-fixed), "unallocated_purchased_anlas": max(0, q.Purchased-purchased),
		"projected_opus_percent":  q.projectedOpusPercent(),
		"allocated_opus_images":   opusAllocatedImages,
		"unallocated_opus_images": max(0, opusAvailableImages-opusAllocatedImages),
		"opus_predicted":          q.OpusPredicted,
		"opus_confirmed_at":       q.Refreshed,
		"opus_next_percent_at":    q.OpusNextPercentAt,
		"active":                  q.Official.Active, "isGracePeriod": q.Official.Grace, "tier": q.Official.Tier,
		"snapshot_age_seconds": int64(time.Since(q.Refreshed).Seconds()),
	}
}
