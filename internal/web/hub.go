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
}

type subscriber struct {
	userID int64
	ch     chan struct{} // buffered(1): bursts of changes coalesce into one signal
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe(userID int64) *subscriber {
	s := &subscriber{userID: userID, ch: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
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
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream

	sub := s.hub.subscribe(u.ID)
	defer s.hub.unsubscribe(sub)

	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second) // keeps idle connections open
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.ch:
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		fl.Flush()
	}
}
