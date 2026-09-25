package proxy

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unversionedStateFixture(t *testing.T, settingsJSON string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range stateTables {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO meta(name,value) VALUES('migrated','1')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO settings(id,data) VALUES(1,?)", settingsJSON); err != nil {
		t.Fatal(err)
	}
	vault := newKeyVault(testAdminKey)
	keyCipher, err := vault.seal("pst-ExistingStateKey12345678")
	if err != nil {
		t.Fatal(err)
	}
	accountCipher, err := vault.seal(testNAIToken)
	if err != nil {
		t.Fatal(err)
	}
	key := clientKey{ID: "existing", Name: "original name", Hash: hexHash("pst-ExistingStateKey12345678"), KeyCiphertext: keyCipher, PolicyVersion: 1, AllowFixed: true, FixedLimit: 100, FixedSpent: 17, FixedPending: 3}
	account := upstreamAccount{ID: "account", Name: "original account", TokenCiphertext: accountCipher}
	job := durableJob{ID: "j_" + strings.Repeat("a", 32), KeyID: key.ID, State: "done", FinishedAt: time.Now(), Status: 200}
	for _, record := range []struct {
		table string
		id    string
		value any
	}{{"keys", key.ID, key}, {"accounts", account.ID, account}, {"jobs", job.ID, job}} {
		data, err := json.Marshal(record.value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO "+record.table+"(id,data) VALUES(?,?)", record.id, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO images(id,group_id,key_id,key_name,ip,created_at,bytes,original,thumbnail) VALUES('image','group','existing','original name','198.51.100.7',?,12,'image.png','image.jpg')", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path+".jobs", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".jobs", job.ID+".result"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openFixtureState(t *testing.T, path string) (*stateDB, *keyStore, *accountStore, *settingsStore, *jobStore, error) {
	t.Helper()
	keys := &keyStore{path: path}
	accounts := &accountStore{path: path + ".accounts.json"}
	settings := &settingsStore{path: path + ".settings.json"}
	jobs := &jobStore{dir: path + ".jobs"}
	db, err := openStateDB(path, keys, accounts, settings, jobs, newKeyVault(testAdminKey))
	return db, keys, accounts, settings, jobs, err
}

func TestUnversionedSQLiteUpgradePreservesState(t *testing.T) {
	path := unversionedStateFixture(t, `{"allow_multi_image":true}`)
	migrated, err := databaseMigrated(path)
	if err != nil || !migrated {
		t.Fatalf("legacy SQLite detection: migrated=%v err=%v", migrated, err)
	}
	db, keys, accounts, settings, jobs, err := openFixtureState(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	if len(keys.keys) != 1 || keys.keys[0].FixedSpent != 17 || keys.keys[0].FixedPending != 3 || len(accounts.accounts) != 1 || len(jobs.jobs) != 1 {
		t.Fatalf("state changed: keys=%+v accounts=%+v jobs=%+v", keys.keys, accounts.accounts, jobs.jobs)
	}
	if settings.data != (proxySettings{AllowMultiImage: true, ArchiveDays: 30, ArchiveMaxBytes: 20 << 30, AdminUIPath: "/console"}) {
		t.Fatalf("old settings defaults: %+v", settings.data)
	}
	var version, imageCount, indexCount int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != stateSchemaVersion {
		t.Fatalf("upgraded schema version=%d err=%v", version, err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM images WHERE id='image' AND ip='198.51.100.7'").Scan(&imageCount); err != nil || imageCount != 1 {
		t.Fatalf("image metadata changed: count=%d err=%v", imageCount, err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('images_created','images_group')").Scan(&indexCount); err != nil || indexCount != 2 {
		t.Fatalf("archive indexes missing: count=%d err=%v", indexCount, err)
	}
	var stored []byte
	if err := db.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(stored, &fields); err != nil || fields["archive_retention_days"] != float64(30) || fields["archive_max_bytes"] != float64(20<<30) {
		t.Fatalf("settings were not persisted with defaults: %s, %v", stored, err)
	}
}

func TestSQLiteUpgradeRejectsUnsupportedOrIncompleteState(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
		change   string
	}{
		{"future version", `{}`, "PRAGMA user_version=99"},
		{"missing table", `{}`, "DROP TABLE images"},
		{"invalid settings", `{"archive_max_bytes":0}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := unversionedStateFixture(t, test.settings)
			if test.change != "" {
				db, err := sql.Open("sqlite", path+".sqlite")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(test.change); err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
			}
			if db, _, _, _, _, err := openFixtureState(t, path); err == nil {
				_ = db.db.Close()
				t.Fatal("accepted incompatible SQLite state")
			}
			db, err := sql.Open("sqlite", path+".sqlite")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count, version int
			if err := db.QueryRow("SELECT COUNT(*) FROM keys WHERE id='existing'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("upgrade failure changed key ledger: count=%d err=%v", count, err)
			}
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 99, false: 0}[test.name == "future version"]; version != want {
				t.Fatalf("upgrade failure changed schema version: %d", version)
			}
		})
	}
}

func TestVersionedSQLiteLoadsMissingSettingsWithDefaults(t *testing.T) {
	path := unversionedStateFixture(t, `{"allow_multi_image":true}`)
	db, _, _, _, _, err := openFixtureState(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE settings SET data=? WHERE id=1", `{"allow_multi_image":true}`); err != nil {
		t.Fatal(err)
	}
	_ = db.db.Close()
	db, _, _, settings, _, err := openFixtureState(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	if settings.data.ArchiveDays != 30 || settings.data.ArchiveMaxBytes != 20<<30 || settings.data.AdminUIPath != "/console" {
		t.Fatalf("versioned SQLite missing defaults: %+v", settings.data)
	}
}

func TestVersionOneSQLiteUpgradePreservesArchiveAndAddsUsage(t *testing.T) {
	path := unversionedStateFixture(t, `{"allow_multi_image":true}`)
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE usage_quarters"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	upgraded, keys, accounts, _, jobs, err := openFixtureState(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.db.Close()
	if len(keys.keys) != 1 || len(accounts.accounts) != 1 || len(jobs.jobs) != 1 {
		t.Fatal("upgrade changed existing state")
	}
	var version, images int
	var ever string
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM images").Scan(&images); err != nil || images != 1 {
		t.Fatalf("archive images=%d err=%v", images, err)
	}
	if err := upgraded.db.QueryRow("SELECT value FROM meta WHERE name='archive_ever'").Scan(&ever); err != nil || ever != "1" {
		t.Fatalf("archive marker=%q err=%v", ever, err)
	}
	if err := upgraded.recordGeneration(time.Date(2026, 9, 25, 1, 25, 0, 0, time.UTC), 2); err != nil {
		t.Fatal(err)
	}
	var generated int
	if err := upgraded.db.QueryRow("SELECT images FROM usage_quarters").Scan(&generated); err != nil || generated != 2 {
		t.Fatalf("usage images=%d err=%v", generated, err)
	}
}
