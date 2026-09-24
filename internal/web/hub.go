package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"doables/internal/store"
)

// hub fans "something changed" signals out to the browsers that are connected
// to /events. It carries no data: a signal just tells the page to refresh
// itself, so what each person sees is always decided by the normal access
// checks.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	// closing is closed when the server shuts down, which ends every stream.
	closing   chan struct{}
	closeOnce sync.Once
}

type subscriber struct {
	userID int64
	ch     chan struct{} // buffered(1): bursts of changes coalesce into one signal
}

// maxStreams is how many live-update connections one person may hold open at
// once. Each is a goroutine and a connection for as long as it lasts, so
// without a cap one token could open thousands. This is more tabs and devices
// than anybody uses; a page over the limit still works, it just stops updating
// by itself until another tab is closed.
const maxStreams = 16

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}, closing: make(chan struct{})} }

// close ends every open stream. Streams never finish by themselves, so a
// graceful shutdown would otherwise wait for them until it gave up.
func (h *hub) close() { h.closeOnce.Do(func() { close(h.closing) }) }

// subscribe registers a stream for userID, or returns nil if they already
// have maxStreams open.
func (h *hub) subscribe(userID int64) *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	open := 0
	for s := range h.subs {
		if s.userID == userID {
			open++
		}
	}
	if open >= maxStreams {
		return nil
	}
	s := &subscriber{userID: userID, ch: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// publish signals the given users, or everyone when all is set. It never blocks.
func (h *hub) publish(userIDs []int64, all bool) {
	want := make(map[int64]bool, len(userIDs))
	for _, id := range userIDs {
		want[id] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if all || want[s.userID] {
			select {
			case s.ch <- struct{}{}:
			default: // a signal is already pending
			}
		}
	}
}

// events streams change signals to one browser as server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.hub.subscribe(u.ID)
	if sub == nil {
		// A browser's EventSource gives up for good on an error status rather
		// than retrying, so this does not turn into a reconnect storm.
		http.Error(w, "too many open live-update connections", http.StatusTooManyRequests)
		return
	}
	defer s.hub.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream

	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second) // keeps idle connections open
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.closing:
			return
		case <-sub.ch:
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		fl.Flush()
	}
}
