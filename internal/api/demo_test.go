package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
	"github.com/Arjunx156/ledgerline/internal/demo"
	"github.com/Arjunx156/ledgerline/internal/payments"
	"github.com/Arjunx156/ledgerline/internal/processor"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

const consoleOrigin = "https://console.example"

type demoOptions struct {
	ttl            time.Duration
	limit          int
	http2          bool
	serverTimeouts time.Duration // read and write; zero leaves them unset
	keepAlive      time.Duration
}

// demoStack is the API in demo mode over real HTTP and a real database.
type demoStack struct {
	db       *dbtest.DB
	url      string
	client   *http.Client
	shutdown func()
}

func newDemoStack(t *testing.T, o demoOptions) *demoStack {
	t.Helper()
	if o.ttl == 0 {
		o.ttl = demo.SessionTTL
	}
	if o.limit == 0 {
		o.limit = 10
	}
	if o.keepAlive == 0 {
		o.keepAlive = 15 * time.Second
	}
	db := dbtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := demo.NewRecorder(db.App, logger)
	t.Cleanup(rec.Close)

	hub := demo.NewHub(db.App.Config().ConnConfig, logger)
	hubDone := make(chan struct{})
	stopping := make(chan struct{})
	go func() { hub.Run(contextUntil(stopping)); close(hubDone) }()

	svc := payments.NewService(db.App, processor.NewSimulator(db.App, 10*time.Second),
		payments.Config{ProcessorTimeout: time.Second})
	router := api.NewRouter(api.Config{DB: db.App, Logger: logger, Payments: svc, Demo: &api.Demo{
		Recorder: rec, Hub: hub, Sessions: demo.NewLimiter(o.limit, time.Hour, time.Now), SessionTTL: o.ttl,
		Origins: []string{consoleOrigin}, ClientIPHeader: "CF-Connecting-IP", KeepAlive: o.keepAlive,
		Shutdown: stopping,
	}})
	srv := httptest.NewUnstartedServer(router)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = o.serverTimeouts, o.serverTimeouts
	if o.http2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	var once sync.Once
	shutdown := func() { once.Do(func() { close(stopping) }) }
	t.Cleanup(func() {
		shutdown()
		srv.Close()
		<-hubDone
	})
	return &demoStack{db: db, url: srv.URL, client: srv.Client(), shutdown: shutdown}
}

// do sends a request as the console page would: from its origin, with the
// sandbox key when there is one.
func (s *demoStack) do(t *testing.T, method, path, key string, body any, header http.Header) response {
	t.Helper()
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.url+path, payload)
	require.NoError(t, err)
	maps(req.Header, header)
	req.Header.Set("Origin", consoleOrigin)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := response{status: resp.StatusCode, header: resp.Header, raw: raw}
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &out.body), "%s", raw)
	}
	return out
}

func maps(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

type session struct {
	id, key, requestID string
}

func (s *demoStack) newSession(t *testing.T, header http.Header) session {
	t.Helper()
	r := s.do(t, "POST", "/demo/sessions", "", map[string]any{}, header)
	require.Equal(t, http.StatusOK, r.status, "%s", r.raw)
	return session{id: r.body["id"].(string), key: r.body["secret_key"].(string), requestID: r.header.Get("Request-Id")}
}

func (s *demoStack) pay(t *testing.T, key, card string) (paymentID, confirmRequest string) {
	t.Helper()
	r := s.do(t, "POST", "/v1/payment_intents", key, map[string]any{"amount": 49900, "currency": "inr"}, nil)
	require.Equal(t, http.StatusOK, r.status, "%s", r.raw)
	paymentID = r.body["id"].(string)
	r = s.do(t, "POST", "/v1/payment_intents/"+paymentID+"/confirm", key, map[string]any{"payment_method": card}, nil)
	require.Equal(t, http.StatusOK, r.status, "%s", r.raw)
	return paymentID, r.header.Get("Request-Id")
}

// sseMessage is one block of a server-sent event stream.
type sseMessage struct {
	id, event, data, retry, comment string
}

type sseStream struct {
	resp     *http.Response
	messages chan sseMessage
}

func (s *demoStack) openStream(t *testing.T, query string, header http.Header) *sseStream {
	t.Helper()
	req, err := http.NewRequest("GET", s.url+"/demo/stream?"+query, nil)
	require.NoError(t, err)
	maps(req.Header, header)
	req.Header.Set("Origin", consoleOrigin)
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, consoleOrigin, resp.Header.Get("Access-Control-Allow-Origin"))

	st := &sseStream{resp: resp, messages: make(chan sseMessage, 1000)}
	go func() {
		defer close(st.messages)
		sc := bufio.NewScanner(resp.Body)
		var m sseMessage
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				st.messages <- m
				m = sseMessage{}
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "":
				m.comment = value
			case "id":
				m.id = value
			case "event":
				m.event = value
			case "data":
				m.data = value
			case "retry":
				m.retry = value
			}
		}
	}()
	return st
}

func (st *sseStream) next(t *testing.T) sseMessage {
	t.Helper()
	select {
	case m, ok := <-st.messages:
		require.True(t, ok, "the stream ended")
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message within 10s")
		return sseMessage{}
	}
}

// event returns the next trace event, skipping keep-alives and the retry
// hint, and checks its SSE id is its seq.
func (st *sseStream) event(t *testing.T) trace.Event {
	t.Helper()
	for {
		m := st.next(t)
		if m.data == "" {
			continue
		}
		require.Empty(t, m.event, "a trace event is an unnamed message")
		var e trace.Event
		require.NoError(t, json.Unmarshal([]byte(m.data), &e))
		require.Equal(t, strconv.FormatInt(e.Seq, 10), m.id)
		return e
	}
}

// until reads trace events up to and including the first that matches.
func (st *sseStream) until(t *testing.T, match func(trace.Event) bool) []trace.Event {
	t.Helper()
	var got []trace.Event
	for {
		e := st.event(t)
		got = append(got, e)
		if match(e) {
			return got
		}
	}
}

func responseTo(requestID string) func(trace.Event) bool {
	return func(e trace.Event) bool { return e.Site == "api.response" && e.RequestID == requestID }
}

func sitesOf(events []trace.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Site
	}
	return out
}

func TestDemoRoutesExistOnlyInDemoMode(t *testing.T) {
	s := newStack(t, nil, nil)
	r := s.doWithAuth(t, "POST", "/demo/sessions", map[string]any{}, "")
	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, "url_unknown", r.errorField("code"))
	r = s.doWithAuth(t, "GET", "/demo/stream?session=ds_x", nil, "")
	assert.Equal(t, http.StatusNotFound, r.status)

	req, err := http.NewRequest("OPTIONS", s.url+"/v1/payment_intents", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", consoleOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "no CORS outside demo mode")
}

// The walking skeleton end to end: a visitor starts a session, opens its
// stream, pays, and watches each step arrive as it happens.
func TestDemoStreamsAPaymentLive(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	r := s.do(t, "POST", "/demo/sessions", "", map[string]any{}, nil)
	require.Equal(t, http.StatusOK, r.status, "%s", r.raw)
	assert.Equal(t, "demo_session", r.body["object"])
	assert.Regexp(t, `^ds_\w{24}$`, r.body["id"])
	assert.Regexp(t, `^sk_test_\w{32}$`, r.body["secret_key"])
	assert.InDelta(t, time.Now().Add(time.Hour).Unix(), r.body["expires_at"], 60)
	sessionID, key := r.body["id"].(string), r.body["secret_key"].(string)
	assert.Equal(t, "/demo/stream?session="+sessionID, r.body["stream_url"])

	st := s.openStream(t, "session="+sessionID, nil)
	assert.Equal(t, "2000", st.next(t).retry, "reconnect after 2s")

	created := st.until(t, responseTo(r.header.Get("Request-Id")))
	assert.Equal(t, []string{"api.request", "demo.session.created", "api.response"}, sitesOf(created),
		"the session's own creation opens its trace, request start included")

	paymentID, confirm := s.pay(t, key, "pm_card_ok")
	events := append(created, st.until(t, responseTo(confirm))...)
	for i, e := range events {
		assert.EqualValues(t, i+1, e.Seq, "seq counts 1, 2, 3 with no gap")
		assert.Equal(t, sessionID, e.Session)
		assert.NotEmpty(t, e.RequestID)
		assert.NotZero(t, e.At)
	}
	var succeeded, approved bool
	for _, e := range events {
		succeeded = succeeded || (e.Site == "payment_intent.transition" && e.PaymentID == paymentID && e.Data["to"] == "succeeded")
		approved = approved || (e.Site == "processor.handled" && e.Data["outcome"] == "approved")
	}
	assert.True(t, succeeded, "the transition to succeeded is in the stream")
	assert.True(t, approved, "the processor's side is in the stream")
}

func TestDemoStreamResumesAfterLastEventID(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	sess := s.newSession(t, nil)

	st := s.openStream(t, "session="+sess.id, http.Header{"Last-Event-Id": {"2"}})
	e := st.event(t)
	assert.EqualValues(t, 3, e.Seq, "a reconnecting browser picks up after the last event it saw")

	r := s.do(t, "GET", "/demo/stream?session="+sess.id, "", nil, http.Header{"Last-Event-Id": {"three"}})
	assert.Equal(t, http.StatusBadRequest, r.status)
	assert.Equal(t, "Last-Event-ID", r.errorField("param"))
}

// Server timeouts are sized for API calls. A stream must outlast them over
// both protocols a browser may arrive on, and keep the line warm meanwhile.
func TestDemoStreamOutlivesServerTimeouts(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run("http2="+strconv.FormatBool(h2), func(t *testing.T) {
			const timeouts = 250 * time.Millisecond
			s := newDemoStack(t, demoOptions{http2: h2, serverTimeouts: timeouts, keepAlive: 100 * time.Millisecond})
			sess := s.newSession(t, nil)
			st := s.openStream(t, "session="+sess.id, nil)
			if h2 {
				assert.Equal(t, 2, st.resp.ProtoMajor)
			}
			st.until(t, responseTo(sess.requestID))

			deadline := time.Now().Add(4 * timeouts)
			keepAlives := 0
			for time.Now().Before(deadline) {
				if st.next(t).comment == "keep-alive" {
					keepAlives++
				}
			}
			assert.GreaterOrEqual(t, keepAlives, 3)

			_, confirm := s.pay(t, sess.key, "pm_card_ok")
			st.until(t, responseTo(confirm))
		})
	}
}

func TestDemoSessionExpires(t *testing.T) {
	s := newDemoStack(t, demoOptions{ttl: 2 * time.Second})
	sess := s.newSession(t, nil)
	st := s.openStream(t, "session="+sess.id, nil)
	st.until(t, responseTo(sess.requestID))

	for {
		m := st.next(t)
		if m.event == "expired" {
			break
		}
		require.Empty(t, m.data, "no trace events after the session's own")
	}
	_, open := <-st.messages
	assert.False(t, open, "the server ends the stream at expiry")

	r := s.do(t, "GET", "/demo/stream?session="+sess.id, "", nil, nil)
	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, "resource_missing", r.errorField("code"))
	assert.Equal(t, "session", r.errorField("param"))

	r = s.do(t, "POST", "/v1/payment_intents", sess.key, map[string]any{"amount": 100, "currency": "inr"}, nil)
	assert.Equal(t, http.StatusUnauthorized, r.status)
	assert.Equal(t, "api_key_expired", r.errorField("code"))
}

func TestDemoSessionsAreLimitedPerAddress(t *testing.T) {
	s := newDemoStack(t, demoOptions{limit: 2})
	from := func(addr string) http.Header { return http.Header{"Cf-Connecting-Ip": {addr}} }

	s.newSession(t, from("203.0.113.7"))
	s.newSession(t, from("203.0.113.7"))
	r := s.do(t, "POST", "/demo/sessions", "", map[string]any{}, from("203.0.113.7"))
	assert.Equal(t, http.StatusTooManyRequests, r.status)
	assert.Equal(t, "rate_limited", r.errorField("code"))
	assert.Contains(t, r.errorField("message"), "Try again in 60 minutes.")
	assert.Equal(t, "3600", r.header.Get("Retry-After"))
	assert.Contains(t, r.header.Get("Access-Control-Expose-Headers"), "Retry-After",
		"the page can read when to try again")

	s.newSession(t, from("203.0.113.8"))

	s.newSession(t, from("2001:db8:1:2::1"))
	s.newSession(t, from("2001:db8:1:2::abcd"))
	r = s.do(t, "POST", "/demo/sessions", "", map[string]any{}, from("2001:db8:1:2:ffff::9"))
	assert.Equal(t, http.StatusTooManyRequests, r.status, "one IPv6 /64 is one visitor")
	s.newSession(t, from("2001:db8:1:3::1"))

	r = s.do(t, "POST", "/demo/sessions", "", map[string]any{"name": "x"}, from("192.0.2.1"))
	assert.Equal(t, http.StatusBadRequest, r.status, "a malformed request doesn't use up the allowance")
	s.newSession(t, from("192.0.2.1"))
	s.newSession(t, from("192.0.2.1"))
}

func TestDemoCORS(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	preflight := func(origin string) *http.Response {
		req, err := http.NewRequest("OPTIONS", s.url+"/v1/payment_intents", nil)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, idempotency-key")
		resp, err := s.client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp
	}

	resp := preflight(consoleOrigin)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, consoleOrigin, resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST", resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Authorization, Content-Type, Idempotency-Key", resp.Header.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", resp.Header.Get("Access-Control-Max-Age"))
	assert.Contains(t, resp.Header.Values("Vary"), "Origin")
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"), "no cookies, ever")

	resp = preflight("https://elsewhere.example")
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	assert.NotEqual(t, http.StatusNoContent, resp.StatusCode)

	r := s.do(t, "GET", "/healthz", "", nil, nil)
	assert.Equal(t, http.StatusOK, r.status)
	assert.Equal(t, consoleOrigin, r.header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, consoleOrigin, r.header.Get("Timing-Allow-Origin"))
	assert.Equal(t, "Request-Id, Retry-After", r.header.Get("Access-Control-Expose-Headers"))
}

func TestDemoStreamEndsOnShutdown(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	sess := s.newSession(t, nil)
	st := s.openStream(t, "session="+sess.id, nil)
	st.until(t, responseTo(sess.requestID))

	s.shutdown()
	for {
		select {
		case _, open := <-st.messages:
			if !open {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the stream outlived shutdown")
		}
	}
}

func TestDemoStreamRefusesBadRequests(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	tests := []struct {
		query, code, param string
		status             int
	}{
		{"", "parameter_missing", "session", http.StatusBadRequest},
		{"session=ds_nobody", "resource_missing", "session", http.StatusNotFound},
		{"session=ds_x&from=0", "parameter_unknown", "from", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			r := s.do(t, "GET", "/demo/stream?"+tt.query, "", nil, nil)
			assert.Equal(t, tt.status, r.status)
			assert.Equal(t, tt.code, r.errorField("code"))
			assert.Equal(t, tt.param, r.errorField("param"))
		})
	}
}

// A key outside any demo session still works on a demo server: its
// request's pending trace is simply never bound.
func TestDemoServesOtherMerchantsAsUsual(t *testing.T) {
	s := newDemoStack(t, demoOptions{})
	_, key, err := auth.CreateMerchant(t.Context(), s.db.App, "Real merchant")
	require.NoError(t, err)
	s.pay(t, key, "pm_card_ok")
}

// contextUntil is cancelled when c closes.
func contextUntil(c <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-c; cancel() }()
	return ctx
}
