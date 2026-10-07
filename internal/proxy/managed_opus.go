package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"sort"
	"time"
)

const opusUnit int64 = 1_000_000
const opusFullUnits = opusFullImages * opusUnit
const opusPercentUnits = opusFullUnits / 100

type opusBucket struct {
	Balance         int64 `json:"balance_units"`
	Pending         int64 `json:"pending_images,omitempty"`
	PredictedCredit int64 `json:"predicted_credit_units,omitempty"`
	Seeded          bool  `json:"seeded"`
}

type opusAccountState struct {
	TokenHash      string    `json:"token_hash"`
	Confirmed      int64     `json:"confirmed_units"`
	Projected      int64     `json:"projected_units"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
	NextPercentAt  time.Time `json:"next_percent_at,omitempty"`
	Predicted      bool      `json:"predicted"`
	PredictedUnits int64     `json:"predicted_units,omitempty"`
}

func opusUnits(percent float64) int64 {
	return int64(math.Floor(math.Max(0, math.Min(100, percent))*float64(opusFullUnits)/100 + 0.0001))
}

func opusShare(keys []clientKey, key clientKey, accountID string, accounts []upstreamAccount) float64 {
	if key.Revoked || !key.AllowOpus || opusMode(key) != "percent" || (keyOpusAccountID(key, accounts) != accountID && keyOpusAccountID(key, accounts) != poolAccountID) {
		return 0
	}
	var fixed, pooled float64
	for _, candidate := range keys {
		if candidate.Revoked || !candidate.AllowOpus || opusMode(candidate) != "percent" {
			continue
		}
		if keyOpusAccountID(candidate, accounts) == accountID {
			fixed += candidate.OpusLimitPercent
		} else if keyOpusAccountID(candidate, accounts) == poolAccountID {
			pooled += candidate.OpusLimitPercent
		}
	}
	if keyOpusAccountID(key, accounts) == accountID {
		return key.OpusLimitPercent / math.Max(100, fixed)
	}
	return math.Max(0, 1-math.Min(1, fixed/100)) * key.OpusLimitPercent / math.Max(100, pooled)
}

func validateOpusShares(before, after []clientKey, accounts []upstreamAccount) error {
	poolTotal := func(keys []clientKey) float64 {
		var sum float64
		for _, key := range keys {
			if !key.Revoked && key.AllowOpus && opusMode(key) == "percent" && keyOpusAccountID(key, accounts) == poolAccountID {
				sum += key.OpusLimitPercent
			}
		}
		return sum
	}
	if next, previous := poolTotal(after), poolTotal(before); next > 100.000001 && next > previous+0.000001 {
		return errors.New("account pool Opus percentages exceed 100")
	}
	for _, account := range accounts {
		count := func(keys []clientKey) float64 {
			var sum float64
			for _, key := range keys {
				if !key.Revoked && key.AllowOpus && opusMode(key) == "percent" && keyOpusAccountID(key, accounts) == account.ID {
					sum += key.OpusLimitPercent
				}
			}
			return sum
		}
		if next, previous := count(after), count(before); next > 100.000001 && next > previous+0.000001 {
			return errors.New("account Opus percentages exceed 100")
		}
	}
	return nil
}

func opusConfiguredOvercommit(keys []clientKey, key clientKey, accounts []upstreamAccount) bool {
	if !key.AllowOpus || opusMode(key) != "percent" {
		return false
	}
	var sum float64
	for _, candidate := range keys {
		if !candidate.Revoked && candidate.AllowOpus && opusMode(candidate) == "percent" && keyOpusAccountID(candidate, accounts) == keyOpusAccountID(key, accounts) {
			sum += candidate.OpusLimitPercent
		}
	}
	return sum > 100.000001
}

func opusCapacity(keys []clientKey, key clientKey, accountID string, accounts []upstreamAccount) int64 {
	return int64(math.Floor(float64(opusFullUnits)*opusShare(keys, key, accountID, accounts) + 0.0001))
}

func bucketFor(key *clientKey, accountID string) opusBucket {
	if key.OpusBuckets == nil {
		key.OpusBuckets = make(map[string]opusBucket)
	}
	return key.OpusBuckets[accountID]
}

func opusAllocated(keys []clientKey, accountID string) int64 {
	var total int64
	for _, key := range keys {
		if key.Revoked || !key.AllowOpus || opusMode(key) != "percent" {
			continue
		}
		if bucket, ok := key.OpusBuckets[accountID]; ok && bucket.Balance > 0 {
			total += bucket.Balance
		}
	}
	return total
}

func distributeOpus(keys []clientKey, accountID string, amount int64, predicted bool, accounts []upstreamAccount) {
	if amount <= 0 {
		return
	}
	for index := range keys {
		share := opusShare(keys, keys[index], accountID, accounts)
		if share <= 0 {
			continue
		}
		bucket := bucketFor(&keys[index], accountID)
		if !bucket.Seeded {
			continue
		}
		credit := int64(math.Floor(float64(amount)*share + 0.0001))
		limit := opusCapacity(keys, keys[index], accountID, accounts)
		credit = min(credit, max(0, limit-bucket.Balance))
		bucket.Balance += credit
		if predicted {
			bucket.PredictedCredit += credit
		}
		keys[index].OpusBuckets[accountID] = bucket
	}
}

func reduceOpus(keys []clientKey, accountID string, projected, loss int64) {
	if loss <= 0 {
		return
	}
	free := max(0, projected-opusAllocated(keys, accountID))
	loss = max(0, loss-free)
	for loss > 0 {
		allocated := opusAllocated(keys, accountID)
		if allocated <= 0 {
			return
		}
		remaining := loss
		for index := range keys {
			bucket, ok := keys[index].OpusBuckets[accountID]
			if !ok || bucket.Balance <= 0 || keys[index].Revoked || !keys[index].AllowOpus || opusMode(keys[index]) != "percent" {
				continue
			}
			debit := max(1, loss*bucket.Balance/allocated)
			debit = min(debit, min(bucket.Balance, remaining))
			bucket.Balance -= debit
			keys[index].OpusBuckets[accountID] = bucket
			remaining -= debit
			if remaining == 0 {
				return
			}
		}
		loss = remaining
	}
}

func seedOpusBuckets(keys []clientKey, accountID string, available int64, accountCount int, accounts []upstreamAccount) {
	for index := range keys {
		if bucket, ok := keys[index].OpusBuckets[accountID]; ok && bucket.Seeded {
			limit := opusCapacity(keys, keys[index], accountID, accounts)
			bucket.Balance = min(bucket.Balance, limit)
			keys[index].OpusBuckets[accountID] = bucket
		}
	}
	for index := range keys {
		share := opusShare(keys, keys[index], accountID, accounts)
		if share <= 0 {
			continue
		}
		if accountCount > 1 && keys[index].PolicyVersion < 2 && keyAccountID(keys[index]) == poolAccountID {
			continue
		}
		bucket := bucketFor(&keys[index], accountID)
		if bucket.Seeded {
			continue
		}
		balance := min(opusCapacity(keys, keys[index], accountID, accounts), int64(math.Floor(float64(available)*share+0.0001)))
		if keys[index].PolicyVersion < 2 {
			legacyRemaining := max(0, opusEffectiveLimit(keys[index])-keys[index].OpusUsed) * opusUnit
			balance = min(balance, legacyRemaining)
		}
		balance = min(balance, max(0, available-opusAllocated(keys, accountID)))
		bucket.Balance = balance
		bucket.Seeded = true
		keys[index].OpusBuckets[accountID] = bucket
		if keys[index].PolicyVersion < 2 {
			keys[index].PolicyVersion = 2
		}
	}
}

func rebalanceOpusBuckets(keys []clientKey, states map[string]opusAccountState, accounts []upstreamAccount) {
	count := 0
	for _, account := range accounts {
		if !account.Disabled && account.provider() == providerNovelAI {
			count++
		}
	}
	if count > 1 {
		allReady := true
		for _, account := range accounts {
			if !account.Disabled && account.provider() == providerNovelAI && states[account.ID].ConfirmedAt.IsZero() {
				allReady = false
				break
			}
		}
		if allReady {
			for index := range keys {
				key := &keys[index]
				if key.PolicyVersion >= 2 || keyAccountID(*key) != poolAccountID || !key.AllowOpus || opusMode(*key) != "percent" {
					continue
				}
				weights := make(map[string]int64)
				var totalWeight int64
				for _, account := range accounts {
					if account.Disabled || account.provider() != providerNovelAI {
						continue
					}
					state := states[account.ID]
					weight := min(opusCapacity(keys, *key, account.ID, accounts), int64(math.Floor(float64(state.Projected)*opusShare(keys, *key, account.ID, accounts)+0.0001)))
					weights[account.ID] = weight
					totalWeight += weight
				}
				if totalWeight == 0 {
					continue
				}
				budget := min(max(0, opusEffectiveLimit(*key)-key.OpusUsed)*opusUnit, totalWeight)
				remaining := budget
				for _, account := range accounts {
					if account.Disabled || account.provider() != providerNovelAI {
						continue
					}
					allocation := budget * weights[account.ID] / totalWeight
					if allocation > remaining {
						allocation = remaining
					}
					allocation = min(allocation, max(0, states[account.ID].Projected-opusAllocated(keys, account.ID)))
					bucket := bucketFor(key, account.ID)
					bucket.Balance = allocation
					bucket.Seeded = true
					key.OpusBuckets[account.ID] = bucket
					remaining -= allocation
				}
				key.PolicyVersion = 2
			}
		}
	}
	for _, account := range accounts {
		if account.Disabled || account.provider() != providerNovelAI {
			continue
		}
		state, ok := states[account.ID]
		if !ok {
			continue
		}
		seedOpusBuckets(keys, account.ID, state.Projected, count, accounts)
		if excess := opusAllocated(keys, account.ID) - state.Projected; excess > 0 {
			reduceOpus(keys, account.ID, opusAllocated(keys, account.ID), excess)
		}
	}
}

func topUpIncreasedOpusShare(before, after []clientKey, states map[string]opusAccountState, accounts []upstreamAccount, keyID string) {
	var previous clientKey
	for _, key := range before {
		if key.ID == keyID {
			previous = key
			break
		}
	}
	if !previous.AllowOpus || opusMode(previous) != "percent" {
		return
	}
	for index := range after {
		key := &after[index]
		if key.ID != keyID || !key.AllowOpus || opusMode(*key) != "percent" {
			continue
		}
		for _, account := range accounts {
			state, ok := states[account.ID]
			if account.Disabled || !ok {
				continue
			}
			increase := opusShare(after, *key, account.ID, accounts) - opusShare(before, previous, account.ID, accounts)
			if increase <= 0 {
				continue
			}
			bucket := key.OpusBuckets[account.ID]
			if !bucket.Seeded {
				continue
			}
			credit := int64(math.Floor(float64(state.Projected)*increase + 0.0001))
			credit = min(credit, max(0, opusCapacity(after, *key, account.ID, accounts)-bucket.Balance))
			credit = min(credit, max(0, state.Projected-opusAllocated(after, account.ID)))
			bucket.Balance += credit
			key.OpusBuckets[account.ID] = bucket
		}
		return
	}
}

func (h *ManagedHandler) syncOpusAccount(accountID, token string, quota upstreamQuota, now time.Time) (*opusAccountState, error) {
	if !quota.OpusKnown {
		return nil, nil
	}
	fingerprint := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(fingerprint[:])
	confirmed := opusUnits(quota.OpusPercent)
	var updated opusAccountState
	err := h.store.updateWithOpus(func(keys []clientKey, accounts map[string]opusAccountState) ([]clientKey, error) {
		state, exists := accounts[accountID]
		if !exists || state.TokenHash != hash || state.ConfirmedAt.IsZero() {
			state = opusAccountState{TokenHash: hash, Confirmed: confirmed, Projected: confirmed}
			for index := range keys {
				delete(keys[index].OpusBuckets, accountID)
			}
		} else {
			if state.Predicted {
				state.Projected -= state.PredictedUnits
				for index := range keys {
					bucket, ok := keys[index].OpusBuckets[accountID]
					if ok {
						bucket.Balance -= bucket.PredictedCredit
						bucket.PredictedCredit = 0
						keys[index].OpusBuckets[accountID] = bucket
					}
				}
			}
			delta := confirmed - state.Projected
			if delta >= opusPercentUnits {
				distributeOpus(keys, accountID, delta/opusPercentUnits*opusPercentUnits, false, h.accounts.snapshot())
			} else if delta < 0 {
				reduceOpus(keys, accountID, state.Projected, -delta)
			}
			state.Confirmed = confirmed
			// A snapshot may still round to the old percent after a local debit.
			// Preserve that debit until the upstream reports a full-percent change.
			if delta > 0 && delta < opusPercentUnits {
				state.Projected = min(state.Projected, confirmed)
			} else {
				state.Projected = confirmed
			}
			state.Predicted = false
			state.PredictedUnits = 0
		}
		state.ConfirmedAt = now
		state.NextPercentAt = time.Time{}
		if quota.NextPercentSeconds > 0 && quota.NextPercentSeconds <= 7*24*3600 && quota.OpusPercent < 100 && (quota.Active || quota.Grace) && quota.Tier == 3 {
			state.NextPercentAt = now.Add(time.Duration(quota.NextPercentSeconds) * time.Second)
		}
		accounts[accountID] = state
		rebalanceOpusBuckets(keys, accounts, h.accounts.snapshot())
		updated = state
		return keys, nil
	})
	return &updated, err
}

func (h *ManagedHandler) predictOpusAccount(accountID string, now time.Time) (*opusAccountState, error) {
	current, ok := h.store.opusSnapshot()[accountID]
	if !ok {
		return nil, nil
	}
	if current.Predicted || current.NextPercentAt.IsZero() || now.Before(current.NextPercentAt) {
		return &current, nil
	}
	var updated opusAccountState
	err := h.store.updateWithOpus(func(keys []clientKey, accounts map[string]opusAccountState) ([]clientKey, error) {
		state, ok := accounts[accountID]
		if !ok {
			return keys, nil
		}
		if !state.Predicted && !state.NextPercentAt.IsZero() && !now.Before(state.NextPercentAt) {
			previous := state.Projected
			state.Projected = min(opusFullUnits, state.Projected+opusPercentUnits)
			state.PredictedUnits = state.Projected - previous
			distributeOpus(keys, accountID, state.PredictedUnits, true, h.accounts.snapshot())
			state.Predicted = true
			accounts[accountID] = state
		}
		updated = state
		return keys, nil
	})
	return &updated, err
}

func (h *ManagedHandler) opusAvailable(key clientKey, accountID string) int64 {
	if !key.AllowOpus || key.Revoked {
		return 0
	}
	if opusMode(key) == "percent" {
		return max(0, key.OpusBuckets[accountID].Balance/opusUnit)
	}
	return opusRemaining(key)
}

func (h *ManagedHandler) opusRemainingForKey(key clientKey) (int64, int64, bool, time.Time) {
	if key.Revoked || !key.AllowOpus {
		return 0, 0, false, time.Time{}
	}
	if opusMode(key) != "percent" {
		return displayRemaining(opusRemaining(key)), opusEffectiveLimit(key), false, time.Time{}
	}
	for _, account := range h.opusAccountCandidates(key) {
		if _, err := h.predictOpusAccount(account.ID, time.Now()); err != nil {
			slog.Warn("Opus prediction unavailable", "account_id", account.ID, "error", err)
		}
	}
	keys := h.store.snapshot()
	states := h.store.opusSnapshot()
	var remaining, capacity int64
	var predicted bool
	var confirmed time.Time
	for _, account := range h.opusAccountCandidates(key) {
		for _, candidate := range keys {
			if candidate.ID == key.ID {
				capacity += opusCapacity(keys, candidate, account.ID, h.accounts.snapshot()) / opusUnit
			}
		}
		state, ok := states[account.ID]
		if !ok {
			continue
		}
		token, err := h.vault.open(account.TokenCiphertext)
		if err != nil {
			continue
		}
		fingerprint := sha256.Sum256([]byte(token))
		if state.TokenHash != hex.EncodeToString(fingerprint[:]) {
			continue
		}
		for _, candidate := range keys {
			if candidate.ID == key.ID {
				remaining += max(0, candidate.OpusBuckets[account.ID].Balance/opusUnit)
			}
		}
		predicted = predicted || state.Predicted
		if confirmed.IsZero() || state.ConfirmedAt.Before(confirmed) {
			confirmed = state.ConfirmedAt
		}
	}
	return remaining, capacity, predicted, confirmed
}

func (h *ManagedHandler) opusRemainingForSubscription(key clientKey) int64 {
	if opusMode(key) == "percent" {
		remaining, _, _, _ := h.opusRemainingForKey(key)
		return remaining
	}
	return opusRemaining(key)
}

func sortedBucketIDs(key clientKey) []string {
	ids := make([]string, 0, len(key.OpusBuckets))
	for id := range key.OpusBuckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func opusPendingByAccount(key clientKey) int64 {
	var total int64
	for _, bucket := range key.OpusBuckets {
		total += bucket.Pending
	}
	return total
}
