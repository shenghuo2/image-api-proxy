package proxy

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type archiveImage struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id"`
	GroupSize int       `json:"group_size"`
	KeyID     string    `json:"key_id"`
	KeyName   string    `json:"key_name"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	Bytes     int64     `json:"bytes"`
}

type archiveIPCount struct {
	IP    string `json:"ip"`
	Count int64  `json:"count"`
}

type archiveHour struct {
	Date  string `json:"date"`
	Hour  int    `json:"hour"`
	Count int64  `json:"count"`
}

type archiveKeySummary struct {
	KeyID    string    `json:"key_id"`
	KeyName  string    `json:"key_name"`
	Count    int64     `json:"count"`
	Bytes    int64     `json:"bytes"`
	IPCount  int64     `json:"ip_count"`
	LatestAt time.Time `json:"latest_at"`
}

func (h *ManagedHandler) serveAdminImages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/images/ips" && r.Method == http.MethodGet {
		h.listAdminImageIPs(w, r)
		return
	}
	if r.URL.Path == "/admin/images/overview" && r.Method == http.MethodGet {
		h.adminImageOverview(w, r)
		return
	}
	if r.URL.Path == "/admin/images/stats" && r.Method == http.MethodGet {
		var count, bytes, failures, pending int64
		var last string
		if err := h.db.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM images").Scan(&count, &bytes); err != nil {
			http.Error(w, "archive unavailable", 500)
			return
		}
		_ = h.db.db.QueryRow("SELECT COUNT(*) FROM archive_work").Scan(&pending)
		_ = h.db.db.QueryRow("SELECT COALESCE((SELECT value FROM meta WHERE name='archive_failures'),'0'),COALESCE((SELECT value FROM meta WHERE name='archive_last_error'),'')").Scan(&failures, &last)
		jsonReply(w, 200, map[string]any{"count": count, "bytes": bytes, "failures": failures, "last_error": last, "pending": pending})
		return
	}
	if r.URL.Path == "/admin/images" && r.Method == http.MethodGet {
		h.listAdminImages(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/images/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !validImageID(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	var original, thumbnail string
	err := h.db.db.QueryRow("SELECT original,thumbnail FROM images WHERE id=?", id).Scan(&original, &thumbnail)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "archive unavailable", 500)
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		h.archive.mu.Lock()
		err = h.archive.deleteImage(id, original, thumbnail)
		h.archive.mu.Unlock()
		if err != nil {
			http.Error(w, "archive unavailable", 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet || len(parts) != 2 || (parts[1] != "thumbnail" && parts[1] != "original") {
		http.NotFound(w, r)
		return
	}
	name := original
	contentType := "image/png"
	if parts[1] == "thumbnail" {
		name = thumbnail
		contentType = "image/jpeg"
	}
	if filepath.Base(name) != name || name == "" {
		http.Error(w, "invalid image path", 500)
		return
	}
	f, err := http.Dir(h.archive.dir).Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if parts[1] == "original" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.png\"", id))
	}
	_, _ = io.Copy(w, f)
}

func (h *ManagedHandler) listAdminImageIPs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		if key != "q" {
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
	}
	search := strings.TrimSpace(q.Get("q"))
	if len(search) > 45 {
		http.Error(w, "invalid IP search", http.StatusBadRequest)
		return
	}
	rows, err := h.db.db.Query("SELECT ip,COUNT(*) FROM images WHERE ip<>'' AND instr(lower(ip),lower(?))>0 GROUP BY ip ORDER BY COUNT(*) DESC,ip LIMIT 501", search)
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	items := []archiveIPCount{}
	for rows.Next() {
		var item archiveIPCount
		if err := rows.Scan(&item.IP, &item.Count); err != nil {
			_ = rows.Close()
			http.Error(w, "archive unavailable", http.StatusInternalServerError)
			return
		}
		items = append(items, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	truncated := len(items) > 500
	if truncated {
		items = items[:500]
	}
	jsonReply(w, http.StatusOK, map[string]any{"items": items, "truncated": truncated})
}

func (h *ManagedHandler) adminImageOverview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		if key != "from" && key != "to" && key != "offset_minutes" {
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
	}
	start, startErr := time.Parse(time.RFC3339, q.Get("from"))
	end, endErr := time.Parse(time.RFC3339, q.Get("to"))
	offset, offsetErr := strconv.Atoi(q.Get("offset_minutes"))
	if startErr != nil || endErr != nil || offsetErr != nil || !end.After(start) || end.Sub(start) > 8*24*time.Hour || offset < -840 || offset > 840 {
		http.Error(w, "invalid time range", http.StatusBadRequest)
		return
	}
	var count, bytes, ipCount, keyCount, groupCount int64
	if err := h.db.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(bytes),0),COUNT(DISTINCT NULLIF(ip,'')),COUNT(DISTINCT key_id),COUNT(DISTINCT group_id) FROM images").Scan(&count, &bytes, &ipCount, &keyCount, &groupCount); err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	rows, err := h.db.db.Query("SELECT strftime('%Y-%m-%d',created_at-?*60,'unixepoch'),CAST(strftime('%H',created_at-?*60,'unixepoch') AS INTEGER),COUNT(*) FROM images WHERE created_at>=? AND created_at<? GROUP BY 1,2 ORDER BY 1,2", offset, offset, start.Unix(), end.Unix())
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	hours := []archiveHour{}
	for rows.Next() {
		var hour archiveHour
		if err := rows.Scan(&hour.Date, &hour.Hour, &hour.Count); err != nil {
			_ = rows.Close()
			http.Error(w, "archive unavailable", http.StatusInternalServerError)
			return
		}
		hours = append(hours, hour)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	rows, err = h.db.db.Query("SELECT key_id,MAX(key_name),COUNT(*),COALESCE(SUM(bytes),0),COUNT(DISTINCT ip),MAX(created_at) FROM images GROUP BY key_id ORDER BY COUNT(*) DESC,key_id")
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	keys := []archiveKeySummary{}
	for rows.Next() {
		var item archiveKeySummary
		var latest int64
		if err := rows.Scan(&item.KeyID, &item.KeyName, &item.Count, &item.Bytes, &item.IPCount, &latest); err != nil {
			_ = rows.Close()
			http.Error(w, "archive unavailable", http.StatusInternalServerError)
			return
		}
		item.LatestAt = time.Unix(latest, 0).UTC()
		keys = append(keys, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		http.Error(w, "archive unavailable", http.StatusInternalServerError)
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"count": count, "bytes": bytes, "ip_count": ipCount, "key_count": keyCount, "group_count": groupCount, "hours": hours, "keys": keys})
}

func validImageID(id string) bool {
	if len(id) != 35 || id[32] != '_' {
		return false
	}
	for i, c := range id {
		if i == 32 {
			continue
		}
		if i > 32 {
			if c < '0' || c > '9' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (h *ManagedHandler) listAdminImages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		if key != "key_id" && key != "ip" && key != "exclude_ip" && key != "from" && key != "to" && key != "page" {
			http.Error(w, "invalid filter", 400)
			return
		}
	}
	page := 1
	if raw := q.Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100000 {
			http.Error(w, "invalid page", 400)
			return
		}
		page = value
	}
	where := []string{"1=1"}
	args := []any{}
	if key := q.Get("key_id"); key != "" {
		where = append(where, "key_id=?")
		args = append(args, key)
	}
	include, exclude := q["ip"], q["exclude_ip"]
	if (len(include) > 0 && len(exclude) > 0) || len(include) > 100 || len(exclude) > 100 {
		http.Error(w, "invalid IP filter", http.StatusBadRequest)
		return
	}
	selected, operator := include, "IN"
	if len(exclude) > 0 {
		selected, operator = exclude, "NOT IN"
	}
	if len(selected) > 0 {
		for _, ip := range selected {
			if ip == "" || len(ip) > 45 {
				http.Error(w, "invalid IP", http.StatusBadRequest)
				return
			}
			args = append(args, ip)
		}
		where = append(where, "ip "+operator+" ("+strings.TrimSuffix(strings.Repeat("?,", len(selected)), ",")+")")
	}
	for _, field := range []string{"from", "to"} {
		if value := q.Get(field); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				http.Error(w, "invalid time", 400)
				return
			}
			op := ">="
			if field == "to" {
				op = "<="
			}
			where = append(where, "created_at"+op+"?")
			args = append(args, parsed.Unix())
		}
	}
	filter := strings.Join(where, " AND ")
	var total int64
	if err := h.db.db.QueryRow("SELECT COUNT(*) FROM images WHERE "+filter, args...).Scan(&total); err != nil {
		http.Error(w, "archive unavailable", 500)
		return
	}
	rows, err := h.db.db.Query("SELECT id,group_id,(SELECT COUNT(*) FROM images AS sibling WHERE sibling.group_id=images.group_id),key_id,key_name,ip,created_at,bytes FROM images WHERE "+filter+" ORDER BY created_at DESC,id DESC LIMIT 24 OFFSET ?", append(args, (page-1)*24)...)
	if err != nil {
		http.Error(w, "archive unavailable", 500)
		return
	}
	defer rows.Close()
	items := []archiveImage{}
	for rows.Next() {
		var item archiveImage
		var created int64
		if err := rows.Scan(&item.ID, &item.GroupID, &item.GroupSize, &item.KeyID, &item.KeyName, &item.IP, &created, &item.Bytes); err != nil {
			http.Error(w, "archive unavailable", 500)
			return
		}
		item.CreatedAt = time.Unix(created, 0).UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "archive unavailable", 500)
		return
	}
	jsonReply(w, 200, map[string]any{"items": items, "page": page, "page_size": 24, "total": total})
}
