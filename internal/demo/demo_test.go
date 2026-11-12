package demo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newSession(t *testing.T, db *dbtest.DB) (Session, string) {
	t.Helper()
	s, key, err := CreateSession(t.Context(), db.App, SessionTTL)
	require.NoError(t, err)
	return s, key
}

func expire(t *testing.T, db *dbtest.DB, id string) {
	t.Helper()
	_, err := db.Owner.Exec(t.Context(), `UPDATE demo_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, id)
	require.NoError(t, err)
}

func TestCreateSessionMakesAWorkingSandboxKey(t *testing.T) {
	db := dbtest.New(t)
	s, key := newSession(t, db)

	got, err := auth.Lookup(t.Context(), db.App, key)
	require.NoError(t, err)
	assert.Equal(t, s.MerchantID, got.MerchantID)
	assert.Equal(t, s.ID, got.Session)
	assert.False(t, got.Operator)
	assert.WithinDuration(t, time.Now().Add(SessionTTL), s.ExpiresAt, time.Minute)

	var sandbox bool
	require.NoError(t, db.Owner.QueryRow(t.Context(), `SELECT sandbox FROM merchants WHERE id = $1`, s.MerchantID).Scan(&sandbox))
	assert.True(t, sandbox)

	found, err := FindSession(t.Context(), db.App, s.ID)
	require.NoError(t, err)
	assert.Equal(t, s.MerchantID, found.MerchantID)
}

// When a session ends, its key and its stream end with it.
func TestExpiredSessionEndsItsKey(t *testing.T) {
	db := dbtest.New(t)
	s, key := newSession(t, db)
	expire(t, db, s.ID)

	_, err := FindSession(t.Context(), db.App, s.ID)
	assert.ErrorIs(t, err, ErrNoSession)
	_, err = auth.Lookup(t.Context(), db.App, key)
	assert.ErrorIs(t, err, auth.ErrExpiredKey)

	_, err = FindSession(t.Context(), db.App, "ds_nobody")
	assert.ErrorIs(t, err, ErrNoSession)
}

// However many requests trace one session at once, its events are numbered
// 1, 2, 3 with no gap, and each request's own events keep their order.
func TestRecorderNumbersEachSessionFromOne(t *testing.T) {
	db := dbtest.New(t)
	a, _ := newSession(t, db)
	b, _ := newSession(t, db)
	var logs bytes.Buffer
	rec := NewRecorder(db.App, slog.New(slog.NewTextHandler(&logs, nil)))

	const goroutines, perGoroutine = 8, 50
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			r := rec.ForSession(a.ID)
			for i := range perGoroutine {
				r.Record(trace.Event{RequestID: fmt.Sprintf("req_%d", g), At: time.Now().UnixMilli(),
					Lane: trace.LaneAPI, Site: "test.step", Data: map[string]any{"i": i}})
			}
		})
	}
	rec.ForSession("ds_gone").Record(trace.Event{Lane: trace.LaneAPI, Site: "test.lost"})
	at := time.UnixMilli(1_700_000_000_123)
	rec.ForSession(b.ID).Record(trace.Event{RequestID: "req_b", At: at.UnixMilli(), Lane: trace.LanePostgres,
		Site: "payment_intent.created", PaymentID: "pi_1", Key: "order-1"})
	wg.Wait()
	rec.Close()
	rec.ForSession(b.ID).Record(trace.Event{Site: "test.after_close"}) // dropped, never blocks

	events, err := EventsAfter(t.Context(), db.App, a.ID, 0, 1000)
	require.NoError(t, err)
	require.Len(t, events, goroutines*perGoroutine)
	next := map[string]int{}
	for i, e := range events {
		assert.EqualValues(t, i+1, e.Seq)
		assert.Equal(t, a.ID, e.Session)
		got := int(e.Data["i"].(float64)) // JSON numbers come back as float64
		assert.Equal(t, next[e.RequestID], got, "%s's events out of order", e.RequestID)
		next[e.RequestID] = got + 1
	}

	events, err = EventsAfter(t.Context(), db.App, b.ID, 0, 1000)
	require.NoError(t, err)
	assert.Equal(t, []trace.Event{{Seq: 1, Session: b.ID, RequestID: "req_b", At: at.UnixMilli(),
		Lane: trace.LanePostgres, Site: "payment_intent.created", PaymentID: "pi_1", Key: "order-1"}}, events)

	assert.Contains(t, logs.String(), "trace events not stored")
	assert.Contains(t, logs.String(), "ds_gone")
}

func TestEventsAfterResumesPastASeq(t *testing.T) {
	db := dbtest.New(t)
	s, _ := newSession(t, db)
	rec := NewRecorder(db.App, discard)
	for range 5 {
		rec.ForSession(s.ID).Record(trace.Event{Lane: trace.LaneAPI, Site: "test.step"})
	}
	rec.Close()

	events, err := EventsAfter(t.Context(), db.App, s.ID, 3, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.EqualValues(t, 4, events[0].Seq)

	events, err = EventsAfter(t.Context(), db.App, s.ID, 0, 2)
	require.NoError(t, err)
	assert.Len(t, events, 2, "limit caps a page")
}

func runHub(t *testing.T, db *dbtest.DB) *Hub {
	t.Helper()
	hub := NewHub(db.App.Config().ConnConfig, discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return hub
}

// hubConnections counts the hub's connections to this test's database.
func hubConnections(t *testing.T, db *dbtest.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, HubApplicationName).Scan(&n))
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 10*time.Second, 20*time.Millisecond, what)
}

func woken(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal("not woken: " + what)
	}
}

func quiet(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
		t.Fatal("woken: " + what)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestHubWakesOnlyTheSessionNotified(t *testing.T) {
	db := dbtest.New(t)
	a, _ := newSession(t, db)
	b, _ := newSession(t, db)
	hub := NewHub(db.App.Config().ConnConfig, discard)
	wakeA, stopA := hub.Subscribe(a.ID)
	defer stopA()
	wakeB, stopB := hub.Subscribe(b.ID)
	defer stopB()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	woken(t, wakeA, "a catches up once LISTEN is open")
	woken(t, wakeB, "b catches up once LISTEN is open")

	rec := NewRecorder(db.App, discard)
	rec.ForSession(a.ID).Record(trace.Event{Lane: trace.LaneAPI, Site: "test.step"})
	rec.Close()
	woken(t, wakeA, "a's event")
	quiet(t, wakeB, "b has no new events")
}

// A free database restarts and drops connections; the stream must not go
// silent when it does.
func TestHubReconnectsAfterLosingItsConnection(t *testing.T) {
	db := dbtest.New(t)
	s, _ := newSession(t, db)
	hub := runHub(t, db)
	wake, stop := hub.Subscribe(s.ID)
	defer stop()
	woken(t, wake, "first LISTEN")

	var killed int
	require.NoError(t, db.Owner.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE pg_terminate_backend(pid))
		FROM pg_stat_activity WHERE application_name = $1 AND datname = current_database()`, HubApplicationName).Scan(&killed))
	require.Equal(t, 1, killed)
	woken(t, wake, "LISTEN again after the connection was killed")

	rec := NewRecorder(db.App, discard)
	rec.ForSession(s.ID).Record(trace.Event{Lane: trace.LaneAPI, Site: "test.step"})
	rec.Close()
	woken(t, wake, "an event after reconnecting")
}

// With nobody watching, the hub holds no connection, so a database that
// scales to zero can sleep.
func TestHubLetsGoOfTheDatabaseWhenIdle(t *testing.T) {
	db := dbtest.New(t)
	s, _ := newSession(t, db)
	hub := runHub(t, db)
	assert.Zero(t, hubConnections(t, db))

	wake, stop := hub.Subscribe(s.ID)
	woken(t, wake, "first LISTEN")
	assert.Equal(t, 1, hubConnections(t, db))

	stop()
	stop() // twice is harmless
	waitFor(t, "connection closed once idle", func() bool { return hubConnections(t, db) == 0 })

	wake, stop = hub.Subscribe(s.ID)
	defer stop()
	woken(t, wake, "listening again for a new subscriber")
}

func TestLimiterSlidesItsWindow(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	l := NewLimiter(3, time.Hour, func() time.Time { return now })

	for i := range 3 {
		ok, _ := l.Allow("203.0.113.1")
		require.True(t, ok, "hit %d", i+1)
		now = now.Add(10 * time.Minute)
	}
	ok, wait := l.Allow("203.0.113.1")
	assert.False(t, ok)
	assert.Equal(t, 30*time.Minute, wait, "until the first hit, at 10:00, leaves the window at 11:00")

	ok, _ = l.Allow("203.0.113.2")
	assert.True(t, ok, "another address has its own allowance")

	now = now.Add(30 * time.Minute) // 11:00 exactly: the first hit is out
	ok, _ = l.Allow("203.0.113.1")
	assert.True(t, ok)
	ok, wait = l.Allow("203.0.113.1")
	assert.False(t, ok)
	assert.Equal(t, 10*time.Minute, wait)
}

func TestLimiterForgetsQuietVisitors(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	l := NewLimiter(1, time.Hour, func() time.Time { return now })
	for i := range 100 {
		l.Allow(fmt.Sprintf("198.51.100.%d", i))
	}
	now = now.Add(2 * time.Hour)
	l.Allow("192.0.2.1")
	assert.Len(t, l.hits, 1, "only the visitor of the last hour is remembered")
}
