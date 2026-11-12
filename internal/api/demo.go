package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/demo"
	"github.com/Arjunx156/ledgerline/internal/logctx"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// SessionTracer gives the recorder for one demo session's trace.
type SessionTracer interface {
	ForSession(id string) trace.Recorder
}

// Demo configures demo mode (amendment 3.13). Only `ledgerline demo` sets
// it: without it no /demo route exists and nothing is traced.
type Demo struct {
	Recorder   *demo.Recorder
	Hub        *demo.Hub
	Sessions   *demo.Limiter // new sessions per visitor address
	SessionTTL time.Duration

	// Origins are the sites allowed to call the API from a browser.
	Origins []string

	// ClientIPHeader is the header in which the platform's proxy reports the
	// visitor's address, such as CF-Connecting-IP. Empty trusts the TCP peer.
	ClientIPHeader string

	// KeepAlive is how often an idle stream sends a comment, so proxies
	// between here and the browser don't close it.
	KeepAlive time.Duration

	// Shutdown is closed when the process is stopping. Open streams end, and
	// browsers reconnect to whichever instance takes over.
	Shutdown <-chan struct{}
}

type demoHandlers struct {
	db  *pgxpool.Pool
	cfg *Demo
}

type demoSessionJSON struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	SecretKey string `json:"secret_key"`
	ExpiresAt int64  `json:"expires_at"`
	StreamURL string `json:"stream_url"`
}

// createSession gives a visitor a sandbox merchant and its key. The
// request's own trace is the first thing in the new session's stream.
func (d demoHandlers) createSession(r *http.Request) (any, error) {
	var p struct{}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	if ok, wait := d.cfg.Sessions.Allow(clientAddress(r, d.cfg.ClientIPHeader)); !ok {
		minutes := max(1, int((wait+time.Minute-1)/time.Minute))
		return nil, apierror.RateLimited(fmt.Sprintf(
			"Too many sandbox sessions from your network in the last hour. Try again in %d minute%s.",
			minutes, plural(minutes)), wait)
	}
	s, key, err := demo.CreateSession(r.Context(), d.db, d.cfg.SessionTTL)
	if err != nil {
		return nil, err
	}
	logctx.Add(r.Context(), "merchant_id", s.MerchantID, "demo_session", s.ID)
	trace.Bind(r.Context(), d.cfg.Recorder.ForSession(s.ID))
	trace.Emit(r.Context(), trace.LanePostgres, "demo.session.created", trace.Fields{Data: map[string]any{
		"expires_at": s.ExpiresAt.Unix(),
	}})
	return demoSessionJSON{
		ID: s.ID, Object: "demo_session", SecretKey: key, ExpiresAt: s.ExpiresAt.Unix(),
		StreamURL: "/demo/stream?session=" + url.QueryEscape(s.ID),
	}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clientAddress is what the session limit counts: the address the
// platform's proxy reports, or the TCP peer when there is no header to
// trust. If the header is configured but missing, every such request lands
// in the proxy's one shared bucket, which fails safe. IPv6 is counted per
// /64, the block one home or machine is given, so rotating addresses inside
// it buys nothing.
func clientAddress(r *http.Request, header string) string {
	var raw string
	if header != "" {
		raw = strings.TrimSpace(r.Header.Get(header))
	}
	if raw == "" {
		raw, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	addr = addr.Unmap()
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

const (
	streamBatch        = 500
	streamWriteTimeout = 10 * time.Second
	streamRetry        = 2 * time.Second
)

// stream sends a session's trace as server-sent events: everything so far,
// then each event as it is stored, until the session expires. The event id
// is its seq, so a browser that reconnects sends Last-Event-ID and resumes
// exactly where it left off. The session id in the URL is the only
// credential, because EventSource cannot send headers; it reads a sandbox's
// trace and moves no money.
func (d demoHandlers) stream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, after, err := streamParams(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s, err := demo.FindSession(ctx, d.db, id)
	if errors.Is(err, demo.ErrNoSession) {
		e := apierror.NotFound("demo_session", id)
		e.Param = "session"
		e.Message += " Sessions last an hour; start a new one with POST /demo/sessions."
		writeError(w, r, e)
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	logctx.Add(ctx, "demo_session", s.ID)

	// Subscribe before the first read, so nothing stored in between is missed.
	wake, unsubscribe := d.cfg.Hub.Subscribe(s.ID)
	defer unsubscribe()

	// The server's write timeout is sized for API calls, so each write to
	// the stream gets a deadline of its own instead.
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	out := &eventStream{w: w, rc: rc}
	fmt.Fprintf(&out.buf, "retry: %d\n\n", streamRetry.Milliseconds())
	if out.flush() != nil {
		return
	}
	expiry := time.NewTimer(time.Until(s.ExpiresAt))
	defer expiry.Stop()
	keepAlive := time.NewTicker(d.cfg.KeepAlive)
	defer keepAlive.Stop()

	for {
		events, err := demo.EventsAfter(ctx, d.db, s.ID, after, streamBatch)
		if err != nil {
			if ctx.Err() == nil {
				logctx.From(ctx).ErrorContext(ctx, "stream: read trace", "err", err)
			}
			return
		}
		for _, e := range events {
			body, err := json.Marshal(e)
			if err != nil {
				logctx.From(ctx).ErrorContext(ctx, "stream: encode event", "err", err, "seq", e.Seq)
				return
			}
			fmt.Fprintf(&out.buf, "id: %d\ndata: %s\n\n", e.Seq, body)
			after = e.Seq
		}
		if out.buf.Len() > 0 && out.flush() != nil {
			return
		}
		if len(events) == streamBatch {
			continue
		}
		// Each wake-up, and each keep-alive as a backstop, re-reads the trace.
		select {
		case <-ctx.Done():
			return
		case <-d.cfg.Shutdown:
			return
		case <-wake:
		case <-keepAlive.C:
			out.buf.WriteString(": keep-alive\n\n")
			if out.flush() != nil {
				return
			}
		case <-expiry.C:
			out.buf.WriteString("event: expired\ndata: {}\n\n")
			_ = out.flush()
			return
		}
	}
}

// streamParams reads ?session= and the Last-Event-ID a reconnecting
// browser sends.
func streamParams(r *http.Request) (session string, after int64, err error) {
	q := r.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(q)) {
		if name != "session" {
			return "", 0, apierror.InvalidRequest("parameter_unknown", name, "Received unknown parameter: "+name+".")
		}
	}
	session = q.Get("session")
	if session == "" {
		return "", 0, apierror.Missing("session")
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		after, err = strconv.ParseInt(last, 10, 64)
		if err != nil || after < 0 {
			return "", 0, apierror.InvalidRequest("parameter_invalid", "Last-Event-ID",
				"Last-Event-ID must be the seq of an event this stream sent.")
		}
	}
	return session, after, nil
}

type eventStream struct {
	w   io.Writer
	rc  *http.ResponseController
	buf bytes.Buffer
}

// flush sends what is buffered now, rather than when a buffer fills.
func (s *eventStream) flush() error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return err
	}
	s.buf.Reset()
	return s.rc.Flush()
}

// cors lets the demo's site, on another origin, call the API from a
// browser. No cookie is ever involved: the page sends the sandbox key as a
// header itself, so there is no ambient credential another site could ride.
func cors(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if !allowed[origin] {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			// Lets the page see which protocol each request used, h2 or h3.
			h.Set("Timing-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.Set("Access-Control-Expose-Headers", "Request-Id, Retry-After")
			next.ServeHTTP(w, r)
		})
	}
}
