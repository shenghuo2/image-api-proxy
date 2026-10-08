package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const jobRetention = 24 * time.Hour

type durableJob struct {
	ID          string      `json:"id"`
	KeyID       string      `json:"key_id"`
	ClientIP    string      `json:"client_ip,omitempty"`
	KeyHash     string      `json:"key_hash"`
	BodyHash    string      `json:"body_hash"`
	Route       string      `json:"route"`
	ContentType string      `json:"content_type"`
	Accept      string      `json:"accept,omitempty"`
	State       string      `json:"state"`
	QueuedAt    time.Time   `json:"queued_at"`
	StartedAt   time.Time   `json:"started_at,omitempty"`
	FinishedAt  time.Time   `json:"finished_at,omitempty"`
	Status      int         `json:"status,omitempty"`
	Headers     http.Header `json:"headers,omitempty"`
}

type jobStatus struct {
	ID         string    `json:"id"`
	Route      string    `json:"route"`
	State      string    `json:"state"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Status     int       `json:"upstream_status,omitempty"`
	ResultURL  string    `json:"result_url,omitempty"`
}

func viewJob(job durableJob) jobStatus {
	view := jobStatus{ID: job.ID, Route: job.Route, State: job.State, QueuedAt: job.QueuedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, Status: job.Status}
	if job.State == "done" {
		view.ResultURL = "/jobs/" + job.ID + "/result"
	}
	return view
}

type jobStore struct {
	mu   sync.Mutex
	dir  string
	jobs map[string]durableJob
	db   *stateDB
}

func openJobStore(dir string) (*jobStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &jobStore{dir: dir, jobs: make(map[string]durableJob)}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validJobID(id) {
			return nil, fmt.Errorf("invalid stored job name %q", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var job durableJob
		if err := json.Unmarshal(data, &job); err != nil || job.ID != id {
			return nil, fmt.Errorf("invalid stored job %q", id)
		}
		s.jobs[id] = job
	}
	return s, nil
}

func validJobID(id string) bool {
	if len(id) != 34 || !strings.HasPrefix(id, "j_") {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}

func (s *jobStore) path(id, ext string) string { return filepath.Join(s.dir, id+ext) }

func writeJobFile(path string, data []byte) error {
	tmp, err := stageJobFile(filepath.Dir(path), data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return commitJobFile(tmp, path)
}

func stageJobFile(dir string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".job-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func commitJobFile(staged, path string) error {
	if err := os.Rename(staged, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *jobStore) saveLocked(job durableJob) error {
	if s.db != nil {
		if err := s.db.saveJob(job); err != nil {
			return err
		}
		s.jobs[job.ID] = job
		return nil
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := writeJobFile(s.path(job.ID, ".json"), data); err != nil {
		return err
	}
	s.jobs[job.ID] = job
	return nil
}

func (s *jobStore) save(job durableJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(job)
}

func (s *jobStore) add(job durableJob, stagedBody string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.jobs[job.ID]; exists {
		return errors.New("job already exists")
	}
	if err := commitJobFile(stagedBody, s.path(job.ID, ".body")); err != nil {
		return err
	}
	if err := s.saveLocked(job); err != nil {
		_ = os.Remove(s.path(job.ID, ".body"))
		return err
	}
	return nil
}

func (s *jobStore) get(id string) (durableJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *jobStore) waiting() []durableJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]durableJob, 0)
	for _, job := range s.jobs {
		if job.State == "waiting" {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].QueuedAt.Equal(jobs[j].QueuedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].QueuedAt.Before(jobs[j].QueuedAt)
	})
	return jobs
}

func (s *jobStore) recover() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.State == "running" {
			job.State = "interrupted"
			job.FinishedAt = time.Now()
			if err := s.saveLocked(job); err != nil {
				return err
			}
		}
		if job.State == "waiting" {
			if _, err := os.Stat(s.path(job.ID, ".body")); err != nil {
				job.State = "interrupted"
				job.FinishedAt = time.Now()
				if err := s.saveLocked(job); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *jobStore) delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		if err := s.db.deleteJob(id); err != nil {
			return err
		}
	} else {
		if err := os.Remove(s.path(id, ".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(s.jobs, id)
	_ = os.Remove(s.path(id, ".body"))
	_ = os.Remove(s.path(id, ".result"))
	return nil
}

func (s *jobStore) prune() {
	s.mu.Lock()
	ids := make([]string, 0)
	for id, job := range s.jobs {
		if !job.FinishedAt.IsZero() && time.Since(job.FinishedAt) > jobRetention {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.delete(id)
	}
}

func (s *jobStore) cleanupOrphans() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".job-") || strings.HasPrefix(name, ".result-") {
			_ = os.Remove(filepath.Join(s.dir, name))
			continue
		}
		id, ext, ok := strings.Cut(name, ".")
		if !ok || !validJobID(id) {
			continue
		}
		job, exists := s.jobs[id]
		if (ext == "body" && (!exists || job.State != "waiting")) || (ext == "result" && (!exists || job.State != "done")) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
	return nil
}

func (h *ManagedHandler) restoreJobs() error {
	if err := h.jobs.recover(); err != nil {
		return err
	}
	h.jobs.prune()
	if err := h.jobs.cleanupOrphans(); err != nil {
		return err
	}
	for _, job := range h.jobs.waiting() {
		t := h.durableTicket(job)
		if err := h.enqueue(t, -1, nil); err != nil {
			return err
		}
		go h.runDurableJob(t, job.ID)
	}
	return nil
}

func (h *ManagedHandler) durableTicket(job durableJob) *ticket {
	ctx, cancel := context.WithCancel(context.Background())
	return &ticket{ctx: ctx, cancel: cancel, id: job.ID, keyID: job.KeyID, route: "POST " + job.Route, queuedAt: job.QueuedAt, ready: make(chan struct{})}
}

func (h *ManagedHandler) serveJobAPI(w http.ResponseWriter, r *http.Request, key clientKey) {
	if r.Method == http.MethodPost {
		path := strings.TrimPrefix(r.URL.Path, "/jobs")
		for i := range routes {
			if routes[i].method == http.MethodPost && routes[i].path == path {
				h.submitDurableJob(w, r, key, &routes[i])
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !validJobID(parts[0]) {
		http.NotFound(w, r)
		return
	}
	job, ok := h.jobs.get(parts[0])
	if !ok || job.KeyID != key.ID {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 1 {
		jsonReply(w, http.StatusOK, viewJob(job))
		return
	}
	if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "result" {
		if job.State != "done" {
			http.Error(w, "result unavailable: "+job.State, http.StatusConflict)
			return
		}
		f, err := os.Open(h.jobs.path(job.ID, ".result"))
		if err != nil {
			http.Error(w, "result unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		for name, values := range job.Headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(job.Status)
		_, _ = io.Copy(w, f)
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		if job.State == "waiting" {
			h.queueMu.Lock()
			found := false
			for _, t := range h.waiting {
				if t.id == job.ID {
					job.State, job.FinishedAt = "canceled", time.Now()
					if err := h.jobs.save(job); err != nil {
						h.queueMu.Unlock()
						http.Error(w, "job state unavailable", http.StatusInternalServerError)
						return
					}
					t.cancel()
					found = true
					break
				}
			}
			h.queueMu.Unlock()
			if !found {
				http.Error(w, "job already started", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if job.State == "running" {
			http.Error(w, "running job cannot be canceled safely", http.StatusConflict)
			return
		}
		if err := h.jobs.delete(job.ID); err != nil {
			http.Error(w, "job deletion failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

func (h *ManagedHandler) submitDurableJob(w http.ResponseWriter, r *http.Request, key clientKey, selected *route) {
	h.jobs.prune()
	select {
	case h.jobStaging <- struct{}{}:
		defer func() { <-h.jobStaging }()
	case <-r.Context().Done():
		return
	}
	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > int(maxBodyBytes) {
		http.Error(w, "invalid or oversized request body", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := estimateJob(selected.path, body, r.Header.Get("Content-Type")); err != nil {
		http.Error(w, "unsupported request parameters", http.StatusBadRequest)
		return
	}
	if key.KeyCiphertext == "" {
		http.Error(w, "rotate legacy key before using durable jobs", http.StatusConflict)
		return
	}
	var id string
	if idem := r.Header.Get("Idempotency-Key"); idem != "" {
		if len(idem) > 128 || strings.ContainsAny(idem, "\r\n") {
			http.Error(w, "invalid idempotency key", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(key.ID + ":" + idem))
		id = "j_" + hex.EncodeToString(sum[:16])
	} else {
		random, err := randomHex(16)
		if err != nil {
			http.Error(w, "job ID unavailable", http.StatusInternalServerError)
			return
		}
		id = "j_" + random
	}
	hash := sha256.Sum256(body)
	job := durableJob{ID: id, KeyID: key.ID, ClientIP: h.clientIP(r), KeyHash: key.Hash, BodyHash: hex.EncodeToString(hash[:]), Route: selected.path, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), State: "waiting", QueuedAt: time.Now()}
	if existing, ok := h.jobs.get(id); ok {
		replyExistingJob(w, existing, job)
		return
	}
	stagedBody, err := stageJobFile(h.jobs.dir, body)
	if err != nil {
		http.Error(w, "job storage unavailable", http.StatusInternalServerError)
		return
	}
	defer os.Remove(stagedBody)
	h.queueMu.Lock()
	if existing, ok := h.jobs.get(id); ok {
		h.queueMu.Unlock()
		replyExistingJob(w, existing, job)
		return
	}
	t := h.durableTicket(job)
	err = h.enqueueLocked(t, keyQueueLimit(key), func() error { return h.jobs.add(job, stagedBody) })
	h.queueMu.Unlock()
	if err != nil {
		t.cancel()
		if errors.Is(err, errQueueFull) || errors.Is(err, errKeyQueueFull) || errors.Is(err, errQueueDraining) {
			h.queueError(w, err)
		} else {
			http.Error(w, "job storage unavailable", http.StatusInternalServerError)
		}
		return
	}
	go h.runDurableJob(t, id)
	w.Header().Set("Location", "/jobs/"+id)
	jsonReply(w, http.StatusAccepted, viewJob(job))
}

func replyExistingJob(w http.ResponseWriter, existing, submitted durableJob) {
	if existing.KeyID != submitted.KeyID || existing.Route != submitted.Route || existing.BodyHash != submitted.BodyHash || existing.ContentType != submitted.ContentType {
		http.Error(w, "idempotency key conflicts with existing job", http.StatusConflict)
		return
	}
	w.Header().Set("Location", "/jobs/"+existing.ID)
	jsonReply(w, http.StatusAccepted, viewJob(existing))
}

type jobResultWriter struct {
	header http.Header
	file   *os.File
	status int
	err    error
}

func (w *jobResultWriter) Header() http.Header { return w.header }
func (w *jobResultWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *jobResultWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.file.Write(data)
	if err != nil {
		w.err = err
	}
	return n, err
}
func (w *jobResultWriter) Flush() {}

func (h *ManagedHandler) runDurableJob(t *ticket, id string) {
	defer t.cancel()
	release, err := h.waitTicket(t)
	if err != nil {
		return
	}
	defer release()
	job, ok := h.jobs.get(id)
	if !ok || job.State != "waiting" {
		return
	}
	job.State, job.StartedAt = "running", time.Now()
	if err := h.jobs.save(job); err != nil {
		return
	}
	finishInterrupted := func() {
		job.State, job.FinishedAt = "interrupted", time.Now()
		_ = h.jobs.save(job)
	}
	var key clientKey
	for _, candidate := range h.store.snapshot() {
		if candidate.ID == job.KeyID && candidate.Hash == job.KeyHash && !candidate.Revoked {
			key = candidate
			break
		}
	}
	if key.ID == "" {
		finishInterrupted()
		return
	}
	raw, err := h.vault.open(key.KeyCiphertext)
	if err != nil {
		finishInterrupted()
		return
	}
	body, err := os.ReadFile(h.jobs.path(id, ".body"))
	if err != nil {
		finishInterrupted()
		return
	}
	f, err := os.CreateTemp(h.jobs.dir, ".result-*")
	if err != nil {
		finishInterrupted()
		return
	}
	defer os.Remove(f.Name())
	request, err := http.NewRequestWithContext(context.WithValue(context.WithValue(t.ctx, archiveClientIPKey{}, job.ClientIP), archiveJobIDKey{}, job.ID), http.MethodPost, job.Route, bytes.NewReader(body))
	if err != nil {
		_ = f.Close()
		finishInterrupted()
		return
	}
	request.Header.Set("Authorization", "Bearer "+raw)
	request = request.WithContext(context.WithValue(request.Context(), requestLogQueuedAtKey{}, job.QueuedAt))
	request.Header.Set("Content-Type", job.ContentType)
	if job.Accept != "" {
		request.Header.Set("Accept", job.Accept)
	}
	var selected *route
	for i := range routes {
		if routes[i].path == job.Route {
			selected = &routes[i]
			break
		}
	}
	if selected == nil {
		_ = f.Close()
		finishInterrupted()
		return
	}
	writer := &jobResultWriter{header: make(http.Header), file: f}
	h.executeJob(writer, request, key, selected)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writer.err != nil || syncErr != nil || closeErr != nil {
		finishInterrupted()
		return
	}
	if err := os.Rename(f.Name(), h.jobs.path(id, ".result")); err != nil {
		finishInterrupted()
		return
	}
	job.State, job.FinishedAt = "done", time.Now()
	job.Status = writer.status
	if job.Status == 0 {
		job.Status = http.StatusOK
	}
	job.Headers = make(http.Header)
	for _, name := range []string{"Content-Type", "Content-Disposition", "Content-Encoding"} {
		if value := writer.header.Get(name); value != "" {
			job.Headers.Set(name, value)
		}
	}
	if err := h.jobs.save(job); err == nil {
		_ = os.Remove(h.jobs.path(id, ".body"))
	}
}
