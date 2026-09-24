package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type proxySettings struct {
	AllowMultiImage bool  `json:"allow_multi_image"`
	ArchiveEnabled  bool  `json:"archive_enabled"`
	ArchiveDays     int   `json:"archive_retention_days"`
	ArchiveMaxBytes int64 `json:"archive_max_bytes"`
}

func defaultProxySettings() proxySettings {
	return proxySettings{ArchiveDays: 30, ArchiveMaxBytes: 20 << 30}
}

func decodeProxySettings(data []byte) (proxySettings, error) {
	value := defaultProxySettings()
	if err := json.Unmarshal(data, &value); err != nil {
		return proxySettings{}, err
	}
	if value.ArchiveDays < -1 || value.ArchiveDays > 36500 || value.ArchiveMaxBytes < 1<<20 || value.ArchiveMaxBytes > 1<<40 {
		return proxySettings{}, errors.New("invalid archive settings")
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
