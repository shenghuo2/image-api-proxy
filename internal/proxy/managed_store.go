package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
)

type clientKey struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Hash             string  `json:"hash"`
	KeyCiphertext    string  `json:"key_ciphertext,omitempty"`
	PolicyVersion    int     `json:"policy_version"`
	AllowFixed       bool    `json:"allow_fixed_anlas"`
	FixedLimit       int64   `json:"fixed_anlas_limit"`
	FixedSpent       int64   `json:"fixed_anlas_spent"`
	FixedPending     int64   `json:"fixed_anlas_pending"`
	AllowPurchased   bool    `json:"allow_purchased_anlas"`
	PurchasedLimit   int64   `json:"purchased_anlas_limit"`
	PurchasedSpent   int64   `json:"purchased_anlas_spent"`
	PurchasedPending int64   `json:"purchased_anlas_pending"`
	AllowOpus        bool    `json:"allow_opus"`
	AllowMultiImage  bool    `json:"allow_multi_image"`
	OpusLimit        int64   `json:"opus_limit_images"`
	OpusLimitMode    string  `json:"opus_limit_mode,omitempty"`
	OpusLimitPercent float64 `json:"opus_limit_percent,omitempty"`
	OpusUsed         int64   `json:"opus_used_images"`
	OpusPending      int64   `json:"opus_pending_images"`
	Revoked          bool    `json:"revoked"`
	Allocated        int64   `json:"allocated,omitempty"`
	Spent            int64   `json:"spent,omitempty"`
	Pending          int64   `json:"pending,omitempty"`
}

type keyStore struct {
	mu   sync.Mutex
	path string
	keys []clientKey
}

func openKeyStore(path string) (*keyStore, error) {
	if path == "" {
		return nil, errors.New("state path is required")
	}
	s := &keyStore{path: path, keys: []clientKey{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.keys); err != nil {
		return nil, fmt.Errorf("decode key state: %w", err)
	}
	for i := range s.keys {
		if s.keys[i].PolicyVersion == 0 {
			s.keys[i].PolicyVersion = 1
			s.keys[i].AllowFixed = true
			s.keys[i].FixedLimit = s.keys[i].Allocated
			s.keys[i].FixedSpent = s.keys[i].Spent
			s.keys[i].FixedPending = s.keys[i].Pending
			s.keys[i].Allocated, s.keys[i].Spent, s.keys[i].Pending = 0, 0, 0
		}
	}
	return s, nil
}

func (s *keyStore) update(fn func([]clientKey) ([]clientKey, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]clientKey(nil), s.keys...)
	next, err := fn(next)
	if err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	s.keys = next
	return nil
}

func (s *keyStore) snapshot() []clientKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]clientKey(nil), s.keys...)
}

func (s *keyStore) find(raw string) (clientKey, bool) {
	hash := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		stored, err := hex.DecodeString(key.Hash)
		if err == nil && len(stored) == len(hash) && subtle.ConstantTimeCompare(stored, hash[:]) == 1 && !key.Revoked {
			return key, true
		}
	}
	return clientKey{}, false
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomClientKey() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for {
		key := make([]byte, 24)
		upper, lower, digit := false, false, false
		for i := 0; i < len(key); {
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				return "", err
			}
			for _, value := range random {
				if value >= 248 {
					continue
				}
				char := alphabet[int(value)%len(alphabet)]
				key[i] = char
				upper = upper || char >= 'A' && char <= 'Z'
				lower = lower || char >= 'a' && char <= 'z'
				digit = digit || char >= '0' && char <= '9'
				i++
				if i == len(key) {
					break
				}
			}
		}
		if upper && lower && digit {
			return "pst-" + string(key), nil
		}
	}
}

func remaining(k clientKey) int64 {
	fixed, purchased := fixedRemaining(k), purchasedRemaining(k)
	if fixed == math.MaxInt64 || purchased == math.MaxInt64 {
		return -1
	}
	return fixed + purchased
}

func fixedRemaining(k clientKey) int64 {
	if k.Revoked || !k.AllowFixed {
		return 0
	}
	if k.FixedLimit == -1 {
		return math.MaxInt64
	}
	if k.FixedLimit <= k.FixedSpent {
		return 0
	}
	return k.FixedLimit - k.FixedSpent
}

func purchasedRemaining(k clientKey) int64 {
	if k.Revoked || !k.AllowPurchased {
		return 0
	}
	if k.PurchasedLimit == -1 {
		return math.MaxInt64
	}
	if k.PurchasedLimit <= k.PurchasedSpent {
		return 0
	}
	return k.PurchasedLimit - k.PurchasedSpent
}

func opusRemaining(k clientKey) int64 {
	if k.Revoked || !k.AllowOpus {
		return 0
	}
	limit := opusEffectiveLimit(k)
	if limit == -1 {
		return math.MaxInt64
	}
	if limit <= k.OpusUsed {
		return 0
	}
	return limit - k.OpusUsed
}

func opusEffectiveLimit(k clientKey) int64 {
	if k.OpusLimitMode == "percent" {
		return int64(math.Floor(k.OpusLimitPercent * opusFullImages / 100))
	}
	return k.OpusLimit
}

func displayRemaining(value int64) int64 {
	if value == math.MaxInt64 {
		return -1
	}
	return value
}

func totalRemaining(keys []clientKey) (fixed, purchased int64) {
	for _, key := range keys {
		if key.FixedLimit != -1 {
			fixed += fixedRemaining(key)
		}
		if key.PurchasedLimit != -1 {
			purchased += purchasedRemaining(key)
		}
	}
	return fixed, purchased
}
