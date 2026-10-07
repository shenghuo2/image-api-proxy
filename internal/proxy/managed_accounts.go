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
const providerNovelAI = "novelai"
const providerNewAPI = "new_api"

type upstreamAccount struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	TokenCiphertext   string   `json:"token_ciphertext"`
	Disabled          bool     `json:"disabled"`
	Provider          string   `json:"provider,omitempty"`
	Origin            string   `json:"origin,omitempty"`
	EnabledModels     []string `json:"enabled_models,omitempty"`
	FallbackAccountID string   `json:"fallback_account_id,omitempty"`
	FallbackHighSteps bool     `json:"fallback_high_steps,omitempty"`
}

func (a upstreamAccount) provider() string {
	if a.Provider == providerNewAPI {
		return providerNewAPI
	}
	return providerNovelAI
}

var newAPIModels = []string{
	"nai-diffusion-4-5-full", "nai-diffusion-4-5-curated",
	"nai-diffusion-5-full", "nai-diffusion-5-curated",
}

func validNewAPIModels(models []string) bool {
	if len(models) == 0 || len(models) > len(newAPIModels) {
		return false
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		valid := false
		for _, allowed := range newAPIModels {
			if model == allowed {
				valid = true
				break
			}
		}
		if !valid || seen[model] {
			return false
		}
		seen[model] = true
	}
	return true
}

func (a upstreamAccount) supports(path, model string) bool {
	if a.provider() != providerNewAPI {
		return true
	}
	if path != "/ai/generate-image" && path != "/ai/generate-image-stream" &&
		path != "/image/ai/generate-image" && path != "/image/ai/generate-image-stream" {
		return false
	}
	base := model
	if len(base) > len("-inpainting") && base[len(base)-len("-inpainting"):] == "-inpainting" {
		base = base[:len(base)-len("-inpainting")]
	}
	for _, enabled := range a.EnabledModels {
		if base == enabled {
			return true
		}
	}
	return false
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

// Opus permission on a relay key applies only to its configured official fallback.
func keyOpusAccountID(k clientKey, accounts []upstreamAccount) string {
	id := keyAccountID(k)
	for _, account := range accounts {
		if account.ID == id && account.provider() == providerNewAPI && account.FallbackAccountID != "" {
			return account.FallbackAccountID
		}
	}
	return id
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
