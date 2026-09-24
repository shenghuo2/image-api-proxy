package proxy

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type stateDB struct{ db *sql.DB }

func databaseMigrated(path string) (bool, error) {
	if _, err := os.Stat(path + ".sqlite"); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		return false, err
	}
	defer db.Close()
	var marker string
	var exists int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, errors.New("incomplete SQLite migration; restore legacy backup and remove the incomplete database")
	}
	err = db.QueryRow("SELECT value FROM meta WHERE name='migrated'").Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("incomplete SQLite migration; restore legacy backup and remove the incomplete database")
	}
	if err != nil {
		return false, err
	}
	if marker != "1" {
		return false, errors.New("unknown database migration version")
	}
	return true, nil
}

func openStateDB(path string, keys *keyStore, accounts *accountStore, settings *settingsStore, jobs *jobStore, vault keyVault) (*stateDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".sqlite", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*stateDB, error) { _ = db.Close(); return nil, err }
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000",
		"CREATE TABLE IF NOT EXISTS meta (name TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS keys (id TEXT PRIMARY KEY, data BLOB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY, data BLOB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, data BLOB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS archive_work (id TEXT PRIMARY KEY, data BLOB NOT NULL, created_at INTEGER NOT NULL)",
		"CREATE TABLE IF NOT EXISTS images (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, key_id TEXT NOT NULL, key_name TEXT NOT NULL, ip TEXT NOT NULL, created_at INTEGER NOT NULL, bytes INTEGER NOT NULL, original TEXT NOT NULL, thumbnail TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS images_created ON images(created_at, id)",
		"CREATE INDEX IF NOT EXISTS images_group ON images(group_id)",
	} {
		if _, err := db.Exec(statement); err != nil {
			return fail(err)
		}
	}
	var marker string
	err = db.QueryRow("SELECT value FROM meta WHERE name='migrated'").Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		if err := validateLegacy(keys, accounts, settings, jobs, vault); err != nil {
			return fail(fmt.Errorf("legacy migration: %w", err))
		}
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		rollback := func(err error) (*stateDB, error) {
			_ = tx.Rollback()
			return fail(fmt.Errorf("legacy migration: %w", err))
		}
		for _, k := range keys.keys {
			if err := putJSON(tx, "keys", k.ID, k); err != nil {
				return rollback(err)
			}
		}
		for _, a := range accounts.accounts {
			if err := putJSON(tx, "accounts", a.ID, a); err != nil {
				return rollback(err)
			}
		}
		data, err := json.Marshal(settings.data)
		if err != nil {
			return rollback(err)
		}
		if _, err = tx.Exec("INSERT INTO settings(id,data) VALUES(1,?)", data); err != nil {
			return rollback(err)
		}
		for _, j := range jobs.jobs {
			if err := putJSON(tx, "jobs", j.ID, j); err != nil {
				return rollback(err)
			}
		}
		if _, err = tx.Exec("INSERT INTO meta(name,value) VALUES('migrated','1')"); err != nil {
			return rollback(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	} else if marker != "1" {
		return fail(errors.New("unknown database migration version"))
	}
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return fail(fmt.Errorf("database integrity check: %s: %w", check, err))
	}
	s := &stateDB{db: db}
	if err := s.load(keys, accounts, settings, jobs); err != nil {
		return fail(err)
	}
	keys.db, accounts.db, settings.db, jobs.db = s, s, s, s
	return s, nil
}

func validateLegacy(keys *keyStore, accounts *accountStore, settings *settingsStore, jobs *jobStore, vault keyVault) error {
	seen := map[string]bool{}
	for _, k := range keys.keys {
		decoded, err := hex.DecodeString(k.Hash)
		if k.ID == "" || len(decoded) != 32 || err != nil || seen[k.ID] {
			return errors.New("invalid or duplicate key")
		}
		if k.KeyCiphertext != "" {
			raw, err := vault.open(k.KeyCiphertext)
			if err != nil || hexHash(raw) != k.Hash {
				return errors.New("invalid encrypted key")
			}
		}
		seen[k.ID] = true
	}
	seen = map[string]bool{}
	for _, a := range accounts.accounts {
		if a.ID == "" || a.TokenCiphertext == "" || seen[a.ID] {
			return errors.New("invalid or duplicate account")
		}
		if _, err := vault.open(a.TokenCiphertext); err != nil {
			return errors.New("invalid encrypted account")
		}
		seen[a.ID] = true
	}
	for _, j := range jobs.jobs {
		if !validJobID(j.ID) || j.KeyID == "" {
			return errors.New("invalid job metadata")
		}
		if j.State == "waiting" {
			body, err := os.ReadFile(jobs.path(j.ID, ".body"))
			if err != nil {
				return err
			}
			hash := sha256.Sum256(body)
			if hex.EncodeToString(hash[:]) != j.BodyHash {
				return errors.New("waiting job body hash mismatch")
			}
		}
		if j.State == "done" {
			if _, err := os.Stat(jobs.path(j.ID, ".result")); err != nil {
				return err
			}
		}
	}
	if settings.data.ArchiveDays < -1 || settings.data.ArchiveMaxBytes < 1<<20 {
		return errors.New("invalid archive settings")
	}
	return nil
}

func putJSON(tx *sql.Tx, table, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO "+table+"(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", id, data)
	return err
}

func (s *stateDB) replaceKeys(keys []clientKey) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM keys"); err != nil {
		return err
	}
	for _, k := range keys {
		if err := putJSON(tx, "keys", k.ID, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *stateDB) replaceAccounts(accounts []upstreamAccount) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM accounts"); err != nil {
		return err
	}
	for _, a := range accounts {
		if err := putJSON(tx, "accounts", a.ID, a); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *stateDB) saveSettings(value proxySettings) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE settings SET data=? WHERE id=1", data)
	return err
}

func (s *stateDB) saveJob(job durableJob) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putJSON(tx, "jobs", job.ID, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *stateDB) deleteJob(id string) error {
	_, err := s.db.Exec("DELETE FROM jobs WHERE id=?", id)
	return err
}

func (s *stateDB) load(keys *keyStore, accounts *accountStore, settings *settingsStore, jobs *jobStore) error {
	load := func(table string, callback func([]byte) error) error {
		rows, err := s.db.Query("SELECT data FROM " + table + " ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				return err
			}
			if err := callback(data); err != nil {
				return fmt.Errorf("decode %s: %w", table, err)
			}
		}
		return rows.Err()
	}
	keys.keys = []clientKey{}
	if err := load("keys", func(data []byte) error {
		var k clientKey
		if err := json.Unmarshal(data, &k); err != nil {
			return err
		}
		keys.keys = append(keys.keys, k)
		return nil
	}); err != nil {
		return err
	}
	accounts.accounts = []upstreamAccount{}
	if err := load("accounts", func(data []byte) error {
		var a upstreamAccount
		if err := json.Unmarshal(data, &a); err != nil {
			return err
		}
		accounts.accounts = append(accounts.accounts, a)
		return nil
	}); err != nil {
		return err
	}
	var settingsData []byte
	if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&settingsData); err != nil {
		return err
	}
	if err := json.Unmarshal(settingsData, &settings.data); err != nil {
		return err
	}
	jobs.jobs = make(map[string]durableJob)
	return load("jobs", func(data []byte) error {
		var j durableJob
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		jobs.jobs[j.ID] = j
		return nil
	})
}
