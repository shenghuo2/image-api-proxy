package proxy

import (
	"errors"
)

type keyPolicyInput struct {
	Name           string `json:"name"`
	Allocation     *int64 `json:"allocation_anlas"`
	AllowFixed     *bool  `json:"allow_fixed_anlas"`
	FixedLimit     *int64 `json:"fixed_anlas_limit"`
	AllowPurchased *bool  `json:"allow_purchased_anlas"`
	PurchasedLimit *int64 `json:"purchased_anlas_limit"`
	AllowOpus      *bool  `json:"allow_opus"`
	OpusLimit      *int64 `json:"opus_limit_images"`
}

func applyPolicy(k *clientKey, input keyPolicyInput) error {
	if input.Allocation != nil {
		if input.FixedLimit != nil {
			return errors.New("allocation_anlas and fixed_anlas_limit are mutually exclusive")
		}
		k.FixedLimit = *input.Allocation
		k.AllowFixed = true
	}
	if input.AllowFixed != nil {
		k.AllowFixed = *input.AllowFixed
	}
	if input.FixedLimit != nil {
		k.FixedLimit = *input.FixedLimit
	}
	if input.AllowPurchased != nil {
		k.AllowPurchased = *input.AllowPurchased
	}
	if input.PurchasedLimit != nil {
		k.PurchasedLimit = *input.PurchasedLimit
	}
	if input.AllowOpus != nil {
		k.AllowOpus = *input.AllowOpus
	}
	if input.OpusLimit != nil {
		k.OpusLimit = *input.OpusLimit
	}
	if k.FixedLimit < 0 || k.FixedLimit > 1e9 || k.PurchasedLimit < 0 || k.PurchasedLimit > 1e9 || k.OpusLimit < 0 || k.OpusLimit > 1e7 {
		return errors.New("invalid quota limit")
	}
	return nil
}

type reservation struct {
	Fixed     int64
	Purchased int64
	Opus      int64
}

func chooseReservation(k clientKey, cost jobCost, q *quotaSnapshot) (reservation, error) {
	var hold reservation
	paid := cost.Full
	opusAccount := q.Official.Tier == 3 && (q.Official.Active || q.Official.Grace)
	if cost.OpusEligible && opusAccount {
		free := !cost.V5
		if cost.V5 {
			if !q.Official.OpusKnown {
				return hold, errors.New("Opus allowance unavailable")
			}
			if !q.Official.OpusNegative {
				if q.projectedOpusPercent() <= 5 {
					return hold, errors.New("Opus allowance too low to classify safely")
				}
				free = true
			}
		}
		if free {
			if opusRemaining(k) < 1 {
				return hold, errors.New("Opus quota unavailable for key")
			}
			hold.Opus = 1
			paid = cost.Extras
		}
	}
	if paid == 0 {
		return hold, nil
	}
	hold.Fixed = min(paid, min(fixedRemaining(k), q.Fixed))
	paid -= hold.Fixed
	hold.Purchased = min(paid, min(purchasedRemaining(k), q.Purchased))
	paid -= hold.Purchased
	if paid > 0 {
		return reservation{}, errors.New("Anlas quota or permission insufficient")
	}
	return hold, nil
}
