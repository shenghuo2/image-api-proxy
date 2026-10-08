package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var errQueueFull = errors.New("queue full")
var errKeyQueueFull = errors.New("key queue full")
var errQueueDraining = errors.New("queue draining")

type ticket struct {
	ctx       context.Context
	cancel    context.CancelFunc
	id        string
	keyID     string
	route     string
	queuedAt  time.Time
	startedAt time.Time
	ready     chan struct{}
}

type queueEntry struct {
	ID        string    `json:"id,omitempty"`
	Position  int       `json:"position,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	KeyName   string    `json:"key_name"`
	Route     string    `json:"route"`
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type queueSnapshot struct {
	Capacity int          `json:"capacity"`
	Active   *queueEntry  `json:"active"`
	Waiting  []queueEntry `json:"waiting"`
}

func (h *ManagedHandler) enter(ctx context.Context, keyID, route string, limit int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithCancel(ctx)
	t := &ticket{ctx: waitCtx, cancel: cancel, keyID: keyID, route: route, queuedAt: time.Now(), ready: make(chan struct{})}
	if err := h.enqueue(t, limit, nil); err != nil {
		cancel()
		return nil, err
	}
	release, err := h.waitTicket(t)
	if err != nil {
		cancel()
		return nil, err
	}
	return func() { release(); cancel() }, nil
}

func (h *ManagedHandler) enqueue(t *ticket, limit int, beforeAppend func() error) error {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	return h.enqueueLocked(t, limit, beforeAppend)
}

func (h *ManagedHandler) enqueueLocked(t *ticket, limit int, beforeAppend func() error) error {
	if h.draining {
		return errQueueDraining
	}
	if h.active != nil {
		if t.keyID != "" && limit >= 0 && h.waitingForKeyLocked(t.keyID) >= limit {
			return errKeyQueueFull
		}
		if len(h.waiting) >= h.queueSize {
			return errQueueFull
		}
	}
	if beforeAppend != nil {
		if err := beforeAppend(); err != nil {
			return err
		}
	}
	if h.active == nil {
		h.active = t
		t.startedAt = time.Now()
		close(t.ready)
	} else {
		h.waiting = append(h.waiting, t)
	}
	return nil
}

func (h *ManagedHandler) waitTicket(t *ticket) (func(), error) {
	select {
	case <-t.ready:
		return func() { h.finish(t) }, nil
	case <-t.ctx.Done():
		h.cancel(t)
		return nil, t.ctx.Err()
	}
}

func (h *ManagedHandler) waitingForKeyLocked(keyID string) int {
	count := 0
	for _, t := range h.waiting {
		if t.keyID == keyID {
			count++
		}
	}
	return count
}

func (h *ManagedHandler) queueLength() int {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	return len(h.waiting)
}

func (h *ManagedHandler) keyQueueLength(keyID string) int {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	return h.waitingForKeyLocked(keyID)
}

func (h *ManagedHandler) finish(t *ticket) {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	if h.active != t {
		return
	}
	h.active = nil
	if h.draining {
		return
	}
	for len(h.waiting) > 0 {
		next := h.waiting[0]
		h.waiting[0] = nil
		h.waiting = h.waiting[1:]
		if next.ctx.Err() != nil {
			continue
		}
		h.active = next
		next.startedAt = time.Now()
		close(next.ready)
		break
	}
}

// BeginDrain lets the active request finish and leaves durable waiters on disk.
func (h *ManagedHandler) BeginDrain() {
	if h.logs != nil {
		h.logs.once.Do(func() { close(h.logs.stop) })
	}
	h.queueMu.Lock()
	h.draining = true
	for _, t := range h.waiting {
		if t.cancel != nil {
			t.cancel()
		}
	}
	h.queueMu.Unlock()
}

func (h *ManagedHandler) WaitActive(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		h.queueMu.Lock()
		active := h.active != nil
		h.queueMu.Unlock()
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *ManagedHandler) cancel(t *ticket) {
	h.queueMu.Lock()
	if h.active == t {
		h.queueMu.Unlock()
		h.finish(t)
		return
	}
	for i, waiting := range h.waiting {
		if waiting == t {
			copy(h.waiting[i:], h.waiting[i+1:])
			h.waiting[len(h.waiting)-1] = nil
			h.waiting = h.waiting[:len(h.waiting)-1]
			break
		}
	}
	h.queueMu.Unlock()
}

func (h *ManagedHandler) queueState() queueSnapshot {
	h.queueMu.Lock()
	state := queueSnapshot{Capacity: h.queueSize, Waiting: make([]queueEntry, 0, len(h.waiting))}
	if h.active != nil {
		entry := queueEntry{ID: h.active.id, KeyID: h.active.keyID, Route: h.active.route, QueuedAt: h.active.queuedAt, StartedAt: h.active.startedAt}
		state.Active = &entry
	}
	for i, t := range h.waiting {
		state.Waiting = append(state.Waiting, queueEntry{ID: t.id, Position: i + 1, KeyID: t.keyID, Route: t.route, QueuedAt: t.queuedAt})
	}
	h.queueMu.Unlock()

	names := make(map[string]string)
	for _, key := range h.store.snapshot() {
		names[key.ID] = key.Name
	}
	nameEntry := func(entry *queueEntry) {
		if entry.KeyID == "" {
			entry.KeyName = "管理员"
		} else if name := names[entry.KeyID]; name != "" {
			entry.KeyName = name
		} else {
			entry.KeyName = entry.KeyID
		}
	}
	if state.Active != nil {
		nameEntry(state.Active)
	}
	for i := range state.Waiting {
		nameEntry(&state.Waiting[i])
	}
	return state
}

func (h *ManagedHandler) serveAdminQueue(w http.ResponseWriter, _ *http.Request) {
	jsonReply(w, http.StatusOK, h.queueState())
}
