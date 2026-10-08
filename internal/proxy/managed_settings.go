package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type proxySettings struct {
	ChargePendingAsSpent bool   `json:"charge_pending_as_spent"`
	AllowMultiImage      bool   `json:"allow_multi_image"`
	ArchiveEnabled       bool   `json:"archive_enabled"`
	ArchiveDays          int    `json:"archive_retention_days"`
	ArchiveMaxBytes      int64  `json:"archive_max_bytes"`
	AdminUIPath          string `json:"admin_ui_path"`
	LogsEnabled          bool   `json:"logs_enabled"`
	LogDays              int    `json:"log_retention_days"`
	LogMaxBytes          int64  `json:"log_max_bytes"`
}

func defaultProxySettings() proxySettings {
	return proxySettings{ArchiveDays: 30, ArchiveMaxBytes: 20 << 30, AdminUIPath: "/console", LogsEnabled: true, LogDays: 7, LogMaxBytes: 100 << 20}
}

func validAdminUIPath(path string) bool {
	if len(path) < 2 || len(path) > 128 || path[0] != '/' || strings.HasSuffix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, c := range segment {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	first := strings.SplitN(path[1:], "/", 2)[0]
	switch first {
	case "admin", "ai", "image", "user", "quota", "healthz", "jobs":
		return false
	}
	return true
}

func decodeProxySettings(data []byte) (proxySettings, error) {
	value := defaultProxySettings()
	if err := json.Unmarshal(data, &value); err != nil {
		return proxySettings{}, err
	}
	if value.ArchiveDays < -1 || value.ArchiveDays > 36500 || value.ArchiveMaxBytes < 1<<20 || value.ArchiveMaxBytes > 1<<40 || !validLogRetention(value) || !validAdminUIPath(value.AdminUIPath) {
		return proxySettings{}, errors.New("invalid proxy settings")
	}
	return value, nil
}

type settingsStore struct {
	mu   sync.Mutex
	path string
	data proxySettings
	db   *stateDB
}

func openSettingsStore(path string) (*settingsStore, error) {
	s := &settingsStore{path: path + ".settings.json", data: defaultProxySettings()}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if s.data, err = decodeProxySettings(data); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *settingsStore) snapshot() proxySettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// The caller holds the generation FIFO so no live reservation can be reconciled.
// Pending amounts are already included in spent; committing never charges twice.
func (s *settingsStore) setWithAccounting(value proxySettings, keys *keyStore) error {
	if !value.ChargePendingAsSpent {
		return s.set(value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if s.db == nil || keys.db != s.db {
		return errors.New("accounting database unavailable")
	}
	next := cloneKeys(keys.keys)
	for i := range next {
		next[i].FixedPending, next[i].PurchasedPending, next[i].OpusPending = 0, 0, 0
		for id, bucket := range next[i].OpusBuckets {
			bucket.Pending = 0
			next[i].OpusBuckets[id] = bucket
		}
	}
	if err := s.db.saveSettingsAndKeys(value, next); err != nil {
		return err
	}
	s.data, keys.keys = value, next
	return nil
}

func (s *settingsStore) set(value proxySettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		if err := s.db.saveSettings(value); err != nil {
			return err
		}
		s.data = value
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(tmp).Encode(value); err != nil {
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
	s.data = value
	return nil
}
