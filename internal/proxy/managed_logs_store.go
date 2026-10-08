package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const logSegmentBytes = 1 << 20
const maxLogEntryBytes = 16 << 10

var logIDPattern = regexp.MustCompile(`^\d{8}-\d{6}\.\d{9}-[a-f0-9]{16}$`)

func validLogRetention(s proxySettings) bool {
	return (s.LogDays == -1 || s.LogDays >= 1 && s.LogDays <= 36500) && s.LogMaxBytes >= 1<<20 && s.LogMaxBytes <= 1<<30
}

type requestLogStore struct {
	mu       sync.Mutex
	dir      string
	settings *settingsStore
	stop     chan struct{}
	once     sync.Once
	failures int64
	lastErr  string
}

type logFile struct {
	name string
	size int64
}

type requestLogStats struct {
	Enabled       bool   `json:"enabled"`
	Bytes         int64  `json:"bytes"`
	MaxBytes      int64  `json:"max_bytes"`
	RetentionDays int    `json:"retention_days"`
	Files         int    `json:"files"`
	Failures      int64  `json:"failures"`
	LastError     string `json:"last_error"`
}

type requestLogFilter struct {
	KeyID, AccountID, Route, Query, Before string
	From, To                               time.Time
	ErrorsOnly                             bool
	Status, Limit                          int
}

type requestLogList struct {
	Items      []requestLog `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func newRequestLogStore(path string, settings *settingsStore) *requestLogStore {
	s := &requestLogStore{dir: path + ".logs", settings: settings, stop: make(chan struct{})}
	_ = s.prune()
	return s
}

func (s *requestLogStore) run() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = s.prune()
		}
	}
}

func (s *requestLogStore) recordError(err error) {
	s.failures++
	// Filesystem errors describe the private log directory, never request data.
	s.lastErr = err.Error()
}

func (s *requestLogStore) filesLocked() ([]logFile, error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(s.dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	files := make([]logFile, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") && logIDPattern.MatchString(strings.TrimSuffix(entry.Name(), ".jsonl")) {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			files = append(files, logFile{entry.Name(), info.Size()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func (s *requestLogStore) pruneLocked(settings proxySettings, now time.Time, incoming int64) ([]logFile, int64, error) {
	files, err := s.filesLocked()
	if err != nil {
		return nil, 0, err
	}
	cutoff := ""
	if settings.LogDays != -1 {
		cutoff = now.UTC().AddDate(0, 0, 1-settings.LogDays).Format("20060102")
	}
	var total int64
	for _, file := range files {
		total += file.size
	}
	for len(files) > 0 && (files[0].name[:8] < cutoff || total+incoming > settings.LogMaxBytes) {
		if err := os.Remove(filepath.Join(s.dir, files[0].name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, total, err
		}
		total -= files[0].size
		files = files[1:]
	}
	return files, total, nil
}

func (s *requestLogStore) prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, err := s.pruneLocked(s.settings.snapshot(), time.Now(), 0)
	if err != nil {
		s.recordError(err)
	}
	return err
}

func (s *requestLogStore) append(entry requestLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.settings.snapshot()
	if !settings.LogsEnabled {
		return nil
	}
	random, err := randomHex(8)
	if err != nil {
		s.recordError(err)
		return err
	}
	// IDs order committed log records, independently of overlapping request start times.
	entry.ID = time.Now().UTC().Format("20060102-150405.000000000-") + random
	data, err := json.Marshal(entry)
	if err == nil && len(data) >= maxLogEntryBytes {
		err = errors.New("request log entry exceeds size limit")
	}
	if err != nil {
		s.recordError(err)
		return err
	}
	data = append(data, '\n')
	files, _, err := s.pruneLocked(settings, time.Now(), int64(len(data)))
	if err != nil {
		s.recordError(err)
		return err
	}
	name := entry.ID + ".jsonl"
	if len(files) > 0 {
		last := files[len(files)-1]
		if last.name[:8] == entry.ID[:8] && last.size+int64(len(data)) <= logSegmentBytes {
			name = last.name
		}
	}
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err == nil {
		err = f.Chmod(0600)
		if err == nil {
			err = repairLogTail(f)
		}
		if err == nil {
			_, err = f.Write(data)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		s.recordError(err)
	}
	return err
}

func repairLogTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	tail := make([]byte, min(int64(maxLogEntryBytes), info.Size()))
	if _, err := file.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return err
	}
	return file.Truncate(info.Size() - int64(len(tail)) + int64(bytes.LastIndexByte(tail, '\n')+1))
}

func (s *requestLogStore) stats() requestLogStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.settings.snapshot()
	files, total, err := s.pruneLocked(settings, time.Now(), 0)
	if err != nil {
		s.recordError(err)
	}
	return requestLogStats{settings.LogsEnabled, total, settings.LogMaxBytes, settings.LogDays, len(files), s.failures, s.lastErr}
}

func (s *requestLogStore) list(ctx context.Context, filter requestLogFilter) (requestLogList, error) {
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	s.mu.Lock()
	files, _, err := s.pruneLocked(s.settings.snapshot(), time.Now(), 0)
	if err != nil {
		s.recordError(err)
	}
	s.mu.Unlock()
	result := requestLogList{Items: []requestLog{}}
	if err != nil {
		return result, err
	}
	// Read a snapshot outside the writer lock so searching cannot stall generation.
	for i := len(files) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		f, err := os.Open(filepath.Join(s.dir, files[i].name))
		if errors.Is(err, os.ErrNotExist) {
			continue // Retention may remove a segment while this snapshot is read.
		}
		if err != nil {
			return result, err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, logSegmentBytes+1))
		_ = f.Close()
		if readErr != nil || len(data) > logSegmentBytes {
			return result, errors.New("request log segment unreadable")
		}
		lines := bytes.Split(data, []byte{'\n'})
		// The last part is empty or an incomplete append; neither is a committed line.
		for j := len(lines) - 2; j >= 0; j-- {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var entry requestLog
			if json.Unmarshal(lines[j], &entry) != nil || !logIDPattern.MatchString(entry.ID) {
				continue
			}
			if filter.Before != "" && entry.ID >= filter.Before || filter.KeyID != "" && entry.KeyID != filter.KeyID || filter.AccountID != "" && entry.AccountID != filter.AccountID || filter.Route != "" && entry.Route != filter.Route || filter.Status != 0 && entry.Status != filter.Status || filter.ErrorsOnly && entry.Outcome == "success" || !filter.From.IsZero() && entry.CreatedAt.Before(filter.From) || !filter.To.IsZero() && entry.CreatedAt.After(filter.To) || filter.Query != "" && !strings.Contains(strings.ToLower(entry.Error+" "+entry.Model+" "+entry.ID+" "+entry.RequestID+" "+entry.JobID), strings.ToLower(filter.Query)) {
				continue
			}
			if len(result.Items) == filter.Limit {
				result.NextCursor = result.Items[len(result.Items)-1].ID
				return result, nil
			}
			result.Items = append(result.Items, entry)
		}
	}
	return result, nil
}
