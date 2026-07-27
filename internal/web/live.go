package web

import (
	"sort"
	"sync"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

// inflightTTL bounds how long an exchange may sit in the inflight set. The
// recorder cannot notify when a response fails to persist (it returns the error
// before reaching the observer), so an entry can be orphaned. Expiring it keeps
// the set from growing without bound; the proxy's own idle deadline is 5 minutes,
// so anything older than this is definitely finished one way or another.
const inflightTTL = 10 * time.Minute

// clientBuffer is how many events a slow dashboard client may fall behind
// before it starts losing them. Dropping is deliberate: the recording path must
// never wait on a browser. A client that misses events refetches the list on
// reconnect, so the durable store stays the source of truth.
const clientBuffer = 64

// Hub fans recorded exchanges out to connected dashboards and tracks the ones
// still in flight. It implements store.Observer.
type Hub struct {
	mu       sync.RWMutex
	clients  map[chan model.Exchange]struct{}
	inflight map[string]model.Exchange
}

func NewHub() *Hub {
	return &Hub{
		clients:  map[chan model.Exchange]struct{}{},
		inflight: map[string]model.Exchange{},
	}
}

// OnExchange receives every committed exchange. It must never block the caller —
// it runs on the proxy's request goroutine.
func (h *Hub) OnExchange(ex model.Exchange) {
	h.mu.Lock()
	if ex.State == model.StateInflight {
		h.inflight[ex.ID] = ex
	} else {
		delete(h.inflight, ex.ID)
	}
	for ch := range h.clients {
		select {
		case ch <- ex:
		default: // client is behind; drop rather than stall the proxy
		}
	}
	h.mu.Unlock()
}

// Inflight returns the exchanges currently mid-flight, newest first, evicting
// any that have outlived their TTL.
func (h *Hub) Inflight() []model.Exchange {
	cutoff := time.Now().Add(-inflightTTL)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.Exchange, 0, len(h.inflight))
	for id, ex := range h.inflight {
		if ex.StartedAt.Before(cutoff) {
			delete(h.inflight, id)
			continue
		}
		out = append(out, ex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

func (h *Hub) subscribe() (<-chan model.Exchange, func()) {
	ch := make(chan model.Exchange, clientBuffer)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}
}
