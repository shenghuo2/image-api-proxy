package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const defaultAccountID = "default"
const poolAccountID = "pool"

type upstreamAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	TokenCiphertext string `json:"token_ciphertext"`
	Disabled        bool   `json:"disabled"`
}

type accountStore struct {
	mu       sync.Mutex
	path     string
	accounts []upstreamAccount
	db       *stateDB
}

func openAccountStore(statePath, bootstrapToken string, vault keyVault) (*accountStore, error) {
	s := &accountStore{path: statePath + ".accounts.json", accounts: []upstreamAccount{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if bootstrapToken != "" {
			ciphertext, err := vault.seal(bootstrapToken)
			if err != nil {
				return nil, err
			}
			s.accounts = []upstreamAccount{{ID: defaultAccountID, Name: "默认账号", TokenCiphertext: ciphertext}}
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.accounts); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *accountStore) snapshot() []upstreamAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]upstreamAccount(nil), s.accounts...)
}

func (s *accountStore) find(id string) (upstreamAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.accounts {
		if account.ID == id {
			return account, true
		}
	}
	return upstreamAccount{}, false
}

func (s *accountStore) update(fn func([]upstreamAccount) ([]upstreamAccount, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := fn(append([]upstreamAccount(nil), s.accounts...))
	if err != nil {
		return err
	}
	if s.db != nil {
		if err := s.db.replaceAccounts(next); err != nil {
			return err
		}
		s.accounts = next
		return nil
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".accounts-*")
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
	s.accounts = next
	return nil
}

func keyAccountID(k clientKey) string {
	if k.AccountID == "" {
		return defaultAccountID
	}
	return k.AccountID
}

func totalRemainingForAccount(keys []clientKey, accountID string) (fixed, purchased int64) {
	for _, key := range keys {
		if keyAccountID(key) != accountID {
			continue
		}
		if key.FixedLimit != -1 {
			fixed += fixedRemaining(key)
		}
		if key.PurchasedLimit != -1 {
			purchased += purchasedRemaining(key)
		}
	}
	return fixed, purchased
}

func totalRemainingForPool(keys []clientKey) (fixed, purchased int64) {
	return totalRemainingForAccount(keys, poolAccountID)
}
