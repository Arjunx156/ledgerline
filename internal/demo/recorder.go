package demo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/trace"
)

// Channel is the Postgres NOTIFY channel; the payload is a session id.
const Channel = "demo_trace"

const (
	recorderBuffer = 4096
	maxBatch       = 256
	writeTimeout   = 5 * time.Second
)

// Recorder stores trace events and announces them with NOTIFY, so a stream
// in any process can follow a session. Record never blocks the request it
// traces: events queue in a buffer that one goroutine writes in batches,
// outside any transaction the request holds, so a request can't wait on a
// connection the writer needs. If the buffer is full the event is dropped
// and the drop is logged, because a trace must never slow a payment down.
type Recorder struct {
	db      *pgxpool.Pool
	logger  *slog.Logger
	events  chan trace.Event
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
}

// NewRecorder starts the writer. Close it to flush what is queued.
func NewRecorder(db *pgxpool.Pool, logger *slog.Logger) *Recorder {
	r := &Recorder{
		db:     db,
		logger: logger,
		events: make(chan trace.Event, recorderBuffer),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go r.run()
	return r
}

// ForSession returns the recorder for one session's requests.
func (r *Recorder) ForSession(id string) trace.Recorder {
	return sessionRecorder{r: r, session: id}
}

// Close writes everything already queued, then stops. Events recorded
// after Close are dropped.
func (r *Recorder) Close() {
	close(r.stop)
	<-r.done
}

type sessionRecorder struct {
	r       *Recorder
	session string
}

func (s sessionRecorder) Record(e trace.Event) {
	e.Session = s.session
	select {
	case s.r.events <- e:
	default:
		s.r.dropped.Add(1)
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	batch := make([]trace.Event, 0, maxBatch)
	for {
		select {
		case e := <-r.events:
			batch = append(batch[:0], e)
		case <-r.stop:
			r.drain(batch[:0])
			return
		}
	fill:
		for len(batch) < maxBatch {
			select {
			case e := <-r.events:
				batch = append(batch, e)
			default:
				break fill
			}
		}
		r.write(batch)
	}
}

func (r *Recorder) drain(batch []trace.Event) {
	for {
		select {
		case e := <-r.events:
			batch = append(batch, e)
			if len(batch) == maxBatch {
				r.write(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				r.write(batch)
			}
			return
		}
	}
}

func (r *Recorder) write(events []trace.Event) {
	if n := r.dropped.Swap(0); n > 0 {
		r.logger.Warn("trace events dropped: the recorder's buffer was full", "count", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	for _, group := range bySession(events) {
		if err := r.writeSession(ctx, group); err != nil {
			r.logger.Error("trace events not stored", "demo_session", group[0].Session, "count", len(group), "err", err)
		}
	}
}

// bySession splits events into runs per session, keeping each session's
// order.
func bySession(events []trace.Event) [][]trace.Event {
	index := map[string]int{}
	var groups [][]trace.Event
	for _, e := range events {
		i, ok := index[e.Session]
		if !ok {
			i = len(groups)
			index[e.Session] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], e)
	}
	return groups
}

var traceColumns = []string{"session", "seq", "request_id", "at", "lane", "site", "payment_id", "key", "data"}

// writeSession numbers one session's events and stores them. The session
// row stays locked from numbering to commit, so another writer can't commit
// a later number first and a reader never sees a gap that fills in later.
func (r *Recorder) writeSession(ctx context.Context, events []trace.Event) error {
	session := events[0].Session
	return pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var last int64
		err := tx.QueryRow(ctx, `UPDATE demo_sessions SET last_seq = last_seq + $2 WHERE id = $1 RETURNING last_seq`,
			session, len(events)).Scan(&last)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no demo session %s", session)
		}
		if err != nil {
			return err
		}
		first := last - int64(len(events)) + 1
		rows := make([][]any, len(events))
		for i, e := range events {
			rows[i] = []any{session, first + int64(i), e.RequestID, time.UnixMilli(e.At), string(e.Lane), e.Site,
				nullable(e.PaymentID), nullable(e.Key), jsonData(e.Data)}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"demo_trace_events"}, traceColumns, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, session)
		return err
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonData(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
