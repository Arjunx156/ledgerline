package trace_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Arjunx156/ledgerline/internal/trace"
)

func sites(events []trace.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Site
	}
	return out
}

func TestPendingHoldsEventsUntilBound(t *testing.T) {
	ctx := trace.WithRecorder(context.Background(), &trace.Pending{})
	trace.Emit(ctx, trace.LaneAPI, "api.request", trace.Fields{})
	trace.Emit(ctx, trace.LaneAPI, "auth.checked", trace.Fields{})

	first, second := &trace.Memory{}, &trace.Memory{}
	trace.Bind(ctx, first)
	trace.Emit(ctx, trace.LaneAPI, "api.response", trace.Fields{})
	trace.Bind(ctx, second)
	trace.Emit(ctx, trace.LaneAPI, "api.done", trace.Fields{})

	assert.Equal(t, []string{"api.request", "auth.checked", "api.response", "api.done"}, sites(first.Events()),
		"held events arrive first and in order, then later ones go straight through")
	assert.Empty(t, second.Events(), "a request is bound once")
}

func TestBindLeavesOtherRecordersAlone(t *testing.T) {
	mem := &trace.Memory{}
	ctx := trace.WithRecorder(context.Background(), mem)
	other := &trace.Memory{}
	trace.Bind(ctx, other)
	trace.Emit(ctx, trace.LaneAPI, "api.request", trace.Fields{})
	assert.Len(t, mem.Events(), 1)
	assert.Empty(t, other.Events())

	trace.Bind(context.Background(), other) // no recorder at all: nothing to do
}
