package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func archiveAdminGet(t *testing.T, h *ManagedHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminArchiveFiltersAndOverview(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	entries := []struct {
		ip, key, group string
		hour           int
	}{
		{"198.51.100.7", "key-a", "group-a", 0},
		{"198.51.100.7", "key-a", "group-a", 1},
		{"203.0.113.24", "key-b", "group-b", 1},
		{"2001:db8::9", "key-b", "group-c", 3},
	}
	for index, item := range entries {
		_, err := h.db.db.Exec("INSERT INTO images(id,group_id,key_id,key_name,ip,created_at,bytes,original,thumbnail) VALUES(?,?,?,?,?,?,?,?,?)", fmt.Sprintf("%032x_%02d", index, index), item.group, item.key, item.key, item.ip, base.Add(time.Duration(item.hour)*time.Hour).Unix(), 1024, "original.png", "thumbnail.jpg")
		if err != nil {
			t.Fatal(err)
		}
	}
	checkCount := func(query string, want int) {
		t.Helper()
		response := archiveAdminGet(t, h, "/admin/images?"+query)
		var list struct {
			Total int `json:"total"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &list) != nil || list.Total != want {
			t.Fatalf("list %q: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	checkCount("ip=198.51.100.7", 2)
	checkCount("ip=198.51.100.7&ip=203.0.113.24", 3)
	checkCount("exclude_ip=198.51.100.7", 2)
	checkCount("exclude_ip=198.51.100.7&exclude_ip=203.0.113.24", 1)
	if response := archiveAdminGet(t, h, "/admin/images?ip=198.51.100.7&exclude_ip=203.0.113.24"); response.Code != 400 {
		t.Fatalf("mixed IP filters accepted: %d", response.Code)
	}
	if response := archiveAdminGet(t, h, "/admin/images?exclude_ip="); response.Code != 400 {
		t.Fatalf("empty IP accepted: %d", response.Code)
	}
	response := archiveAdminGet(t, h, "/admin/images/ips")
	var ips struct {
		Items     []archiveIPCount `json:"items"`
		Truncated bool             `json:"truncated"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &ips) != nil || len(ips.Items) != 3 || ips.Items[0].IP != "198.51.100.7" || ips.Items[0].Count != 2 || ips.Truncated {
		t.Fatalf("IP options: status=%d body=%s", response.Code, response.Body.String())
	}
	response = archiveAdminGet(t, h, "/admin/images/ips?q="+url.QueryEscape("2001:DB8"))
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &ips) != nil || len(ips.Items) != 1 || ips.Items[0].IP != "2001:db8::9" {
		t.Fatalf("IP search: status=%d body=%s", response.Code, response.Body.String())
	}
	params := url.Values{"from": {base.Format(time.RFC3339)}, "to": {base.Add(24 * time.Hour).Format(time.RFC3339)}, "offset_minutes": {"-480"}}
	response = archiveAdminGet(t, h, "/admin/images/overview?"+params.Encode())
	var overview struct {
		Count      int64               `json:"count"`
		Bytes      int64               `json:"bytes"`
		IPCount    int64               `json:"ip_count"`
		KeyCount   int64               `json:"key_count"`
		GroupCount int64               `json:"group_count"`
		Hours      []archiveHour       `json:"hours"`
		Keys       []archiveKeySummary `json:"keys"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &overview) != nil {
		t.Fatalf("overview: status=%d body=%s", response.Code, response.Body.String())
	}
	if overview.Count != 4 || overview.Bytes != 4096 || overview.IPCount != 3 || overview.KeyCount != 2 || overview.GroupCount != 3 || len(overview.Keys) != 2 || len(overview.Hours) != 3 {
		t.Fatalf("overview counts: %+v", overview)
	}
	if overview.Hours[0].Date != "2026-09-25" || overview.Hours[0].Hour != 0 || overview.Hours[1].Hour != 1 || overview.Hours[1].Count != 2 {
		t.Fatalf("overview hours: %+v", overview.Hours)
	}
	if response := archiveAdminGet(t, h, "/admin/images/overview?from=bad&to=bad&offset_minutes=0"); response.Code != 400 {
		t.Fatalf("invalid overview accepted: %d", response.Code)
	}
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/images/ips", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("IP options leaked: %d", unauthorized.Code)
	}
}

func TestAdminArchiveIPListLimit(t *testing.T) {
	h, err := NewManaged(ManagedConfig{AdminKey: testAdminKey, NovelAIToken: testNAIToken, StatePath: filepath.Join(t.TempDir(), "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := range 501 {
		ip := fmt.Sprintf("2001:db8::%x", index)
		_, err := tx.Exec("INSERT INTO images(id,group_id,key_id,key_name,ip,created_at,bytes,original,thumbnail) VALUES(?,?,?,?,?,?,?,?,?)", fmt.Sprintf("%032x_%02d", index, index%100), "group", "key", "key", ip, 1, 1, "a", "b")
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	response := archiveAdminGet(t, h, "/admin/images/ips")
	var result struct {
		Items     []archiveIPCount `json:"items"`
		Truncated bool             `json:"truncated"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 500 || !result.Truncated {
		t.Fatalf("limited options: status=%d count=%d truncated=%v", response.Code, len(result.Items), result.Truncated)
	}
	response = archiveAdminGet(t, h, "/admin/images/ips?q="+url.QueryEscape("2001:db8::1f4"))
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 1 || result.Items[0].IP != "2001:db8::1f4" {
		t.Fatalf("search beyond limit: status=%d body=%s", response.Code, response.Body.String())
	}
}
