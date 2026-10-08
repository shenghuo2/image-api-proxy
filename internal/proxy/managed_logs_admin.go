package proxy

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func (h *ManagedHandler) serveAdminLogs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/logs/stats" && r.Method == http.MethodGet {
		jsonReply(w, http.StatusOK, h.logs.stats())
		return
	}
	if r.URL.Path == "/admin/logs" && r.Method == http.MethodDelete {
		if len(r.URL.Query()) != 0 {
			http.Error(w, "log deletion does not accept filters", http.StatusBadRequest)
			return
		}
		if err := h.logs.clear(); err != nil {
			http.Error(w, "request logs unavailable", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/admin/logs" || r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	for name, values := range q {
		switch name {
		case "key_id", "account_id", "route", "q", "before", "from", "to", "errors_only", "status", "limit":
		default:
			http.Error(w, "unsupported log filter", http.StatusBadRequest)
			return
		}
		if len(values) != 1 || len(values[0]) > 200 {
			http.Error(w, "invalid log filter", http.StatusBadRequest)
			return
		}
	}
	filter := requestLogFilter{KeyID: q.Get("key_id"), AccountID: q.Get("account_id"), Route: q.Get("route"), Query: q.Get("q"), Before: q.Get("before"), Limit: 50}
	if filter.Before != "" && !logIDPattern.MatchString(filter.Before) {
		http.Error(w, "invalid log cursor", http.StatusBadRequest)
		return
	}
	for name, target := range map[string]*time.Time{"from": &filter.From, "to": &filter.To} {
		if value := q.Get(name); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				http.Error(w, "invalid log date", http.StatusBadRequest)
				return
			}
			*target = parsed
		}
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.From.After(filter.To) {
		http.Error(w, "invalid log date range", http.StatusBadRequest)
		return
	}
	if value := q.Get("errors_only"); value != "" {
		if value != "true" && value != "false" {
			http.Error(w, "invalid error filter", http.StatusBadRequest)
			return
		}
		filter.ErrorsOnly = value == "true"
	}
	if value := q.Get("status"); value != "" {
		status, err := strconv.Atoi(value)
		if err != nil || status < 100 || status > 599 {
			http.Error(w, "invalid status filter", http.StatusBadRequest)
			return
		}
		filter.Status = status
	}
	if value := q.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			http.Error(w, "invalid log limit", http.StatusBadRequest)
			return
		}
		filter.Limit = limit
	}
	result, err := h.logs.list(r.Context(), filter)
	if err != nil {
		http.Error(w, "request logs unavailable", http.StatusInternalServerError)
		return
	}
	jsonReply(w, http.StatusOK, result)
}

func (s *requestLogStore) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.filesLocked()
	if err == nil {
		for _, file := range files {
			if err = os.Remove(filepath.Join(s.dir, file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				break
			}
			err = nil
		}
	}
	if err != nil {
		s.recordError(err)
	}
	return err
}
