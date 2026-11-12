package demo

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// HubApplicationName labels the hub's connection in pg_stat_activity.
const HubApplicationName = "ledgerline-demo-hub"

const maxReconnectWait = 30 * time.Second

// Hub shares one LISTEN connection among every open stream in the process
// and wakes the streams of the session each notification names.
//
// It listens only while someone is subscribed. A free Postgres that scales
// to zero when idle would otherwise be kept awake, or woken again each time
// it suspends and drops the connection.
type Hub struct {
	cfg    *pgx.ConnConfig
	logger *slog.Logger

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
	n    int
	idle context.CancelFunc // ends the current LISTEN once nobody is subscribed
	kick chan struct{}
}

// NewHub listens with connections made from cfg, which must reach Postgres
// directly: LISTEN does not survive a transaction-mode pooler.
func NewHub(cfg *pgx.ConnConfig, logger *slog.Logger) *Hub {
	cfg = cfg.Copy()
	cfg.RuntimeParams["application_name"] = HubApplicationName
	return &Hub{
		cfg:    cfg,
		logger: logger,
		subs:   map[string]map[chan struct{}]struct{}{},
		kick:   make(chan struct{}, 1),
	}
}

// Subscribe returns a channel that receives whenever the session may have
// new events, and a function that ends the subscription. A wake-up is a
// hint, not an event: it holds at most one pending value, so a slow reader
// gets one wake for many events and catches up with a single query.
func (h *Hub) Subscribe(session string) (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[session] == nil {
		h.subs[session] = map[chan struct{}]struct{}{}
	}
	h.subs[session][wake] = struct{}{}
	h.n++
	h.mu.Unlock()
	nudge(h.kick)

	var once sync.Once
	return wake, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[session], wake)
			if len(h.subs[session]) == 0 {
				delete(h.subs, session)
			}
			h.n--
			if h.n == 0 && h.idle != nil {
				h.idle()
			}
		})
	}
}

// Run keeps a LISTEN open while anyone is subscribed, reconnecting after a
// failure, until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	var wait time.Duration
	for {
		if !h.awaitSubscribers(ctx) {
			return
		}
		listened, err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			wait = 0
			continue
		}
		// A connection that worked and then broke is retried at once; one that
		// can't be made at all backs off.
		if listened {
			wait = 0
		} else {
			wait = min(max(2*wait, time.Second), maxReconnectWait)
		}
		h.logger.Warn("trace listener lost its connection", "err", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (h *Hub) awaitSubscribers(ctx context.Context) bool {
	for {
		h.mu.Lock()
		n := h.n
		h.mu.Unlock()
		if n > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-h.kick:
		}
	}
}

// listen holds one LISTEN until nobody is subscribed (nil) or the
// connection fails. listened reports whether LISTEN was established.
func (h *Hub) listen(ctx context.Context) (listened bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, h.cfg)
	if err != nil {
		return false, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return false, err
	}

	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.mu.Lock()
	if h.n == 0 {
		h.mu.Unlock()
		return true, nil
	}
	h.idle = cancel
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.idle = nil
		h.mu.Unlock()
	}()

	// Anything committed while no LISTEN was open raised no notification.
	h.wakeAll()
	for {
		n, err := conn.WaitForNotification(listenCtx)
		if err != nil {
			if listenCtx.Err() != nil && ctx.Err() == nil {
				return true, nil
			}
			return true, err
		}
		h.wake(n.Payload)
	}
}

func (h *Hub) wake(session string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs[session] {
		nudge(c)
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subs {
		for c := range subs {
			nudge(c)
		}
	}
}

// nudge leaves one value in c unless one is already waiting.
func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
