// Package trace records what the system did, step by step, for the proof
// console. The event shape is the contract between backend and console
// (amendment 3.13): it changes only together with the console.
//
// Emitting is free when nothing listens: production requests carry no
// recorder, and Emit returns at once.
package trace

import (
	"context"
	"sync"
	"time"
)

// Lane is the swimlane an event is drawn in.
type Lane string

const (
	LaneClient    Lane = "client"
	LaneAPI       Lane = "api"
	LanePostgres  Lane = "postgres"
	LaneProcessor Lane = "processor"
	LaneWorker    Lane = "worker"
	LaneReceiver  Lane = "receiver"
)

// Event is one step. Site is a stable dotted code such as
// payment_intent.transition; the console keys its explanations on it.
type Event struct {
	Seq       int64          `json:"seq"`
	Session   string         `json:"session"`
	RequestID string         `json:"request_id"`
	At        int64          `json:"at"` // unix milliseconds
	Lane      Lane           `json:"lane"`
	Site      string         `json:"site"`
	PaymentID string         `json:"payment_id,omitempty"`
	Key       string         `json:"key,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Fields are the optional parts of an event.
type Fields struct {
	PaymentID string
	Key       string
	Data      map[string]any
}

// Recorder keeps events. It assigns Seq and Session.
type Recorder interface {
	Record(Event)
}

type recorderKey struct{}
type requestKey struct{}

// WithRecorder makes every Emit under ctx go to r.
func WithRecorder(ctx context.Context, r Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// WithRequest stamps later events with the request they belong to.
func WithRequest(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestKey{}, requestID)
}

// Emit records one step, if anything is recording.
func Emit(ctx context.Context, lane Lane, site string, f Fields) {
	r, ok := ctx.Value(recorderKey{}).(Recorder)
	if !ok {
		return
	}
	requestID, _ := ctx.Value(requestKey{}).(string)
	r.Record(Event{
		RequestID: requestID,
		At:        time.Now().UnixMilli(),
		Lane:      lane,
		Site:      site,
		PaymentID: f.PaymentID,
		Key:       f.Key,
		Data:      f.Data,
	})
}

// Memory keeps events in order, for tests and recorded fixtures.
type Memory struct {
	mu     sync.Mutex
	events []Event
}

func (m *Memory) Record(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.Seq = int64(len(m.events) + 1)
	m.events = append(m.events, e)
}

// Events returns a copy of everything recorded so far.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}
