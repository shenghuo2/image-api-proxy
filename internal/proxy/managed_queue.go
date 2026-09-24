package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var errQueueFull = errors.New("queue full")
var errKeyQueueFull = errors.New("key queue full")

type ticket struct {
	ctx       context.Context
	keyID     string
	route     string
	queuedAt  time.Time
	startedAt time.Time
	ready     chan struct{}
}

type queueEntry struct {
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
	t := &ticket{ctx: ctx, keyID: keyID, route: route, queuedAt: time.Now(), ready: make(chan struct{})}
	h.queueMu.Lock()
	if h.active == nil {
		h.active = t
		t.startedAt = time.Now()
		close(t.ready)
	} else {
		if keyID != "" && limit >= 0 && h.waitingForKeyLocked(keyID) >= limit {
			h.queueMu.Unlock()
			return nil, errKeyQueueFull
		}
		if len(h.waiting) >= h.queueSize {
			h.queueMu.Unlock()
			return nil, errQueueFull
		}
		h.waiting = append(h.waiting, t)
	}
	h.queueMu.Unlock()

	select {
	case <-t.ready:
		return func() { h.finish(t) }, nil
	case <-ctx.Done():
		h.cancel(t)
		return nil, ctx.Err()
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
		entry := queueEntry{KeyID: h.active.keyID, Route: h.active.route, QueuedAt: h.active.queuedAt, StartedAt: h.active.startedAt}
		state.Active = &entry
	}
	for i, t := range h.waiting {
		state.Waiting = append(state.Waiting, queueEntry{Position: i + 1, KeyID: t.keyID, Route: t.route, QueuedAt: t.queuedAt})
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
