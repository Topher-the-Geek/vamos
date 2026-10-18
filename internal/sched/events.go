package sched

import (
	"context"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// Events receives a run's progress, within the executor: a job sends them on as api.Events.
// Its methods may be called from several goroutines.
type Events interface {
	// Status reports what is happening to node, such as "building" or "cached".
	Status(node graph.Ref, status string)
	// Output passes on what node's run() steps print,
	// or printed when its cached result was made, to stream: cache.StreamStdout or cache.StreamStderr.
	// data is valid only during the call.
	Output(node graph.Ref, stream byte, data []byte)
	// Spans passes on spans of a trace that the client asked for, in batches, as they end.
	Spans(spans []tracing.Span)
}

// executorProcess names the executor's spans' process in a trace.
const executorProcess = "executor"

// spanBatchSize is how many spans an executor gathers before sending them,
// so that a remote one needn't send a message for each.
const spanBatchSize = 256

// ForCall returns a context whose spans, if call asks for them, go to ev, in batches,
// and a function that sends what is left of them, which the call must run, last, before it returns.
func ForCall(ctx context.Context, call *tracing.CallContext, ev Events) (context.Context, func()) {
	if call == nil {
		return ctx, func() {}
	}
	b := &spanBatch{ev: ev}
	return tracing.ForCall(ctx, call, b.add, executorProcess), b.flush
}

// spanBatch gathers spans to send to events.
// It detaches batches under mu and calls Events.Spans after unlocking,
// so callbacks may overlap and batches may arrive out of completion order.
// Detached slices are never reused.
// The owner must stop producers before the final flush.
type spanBatch struct {
	ev    Events
	mu    sync.Mutex
	spans []tracing.Span
}

// add gathers s, sending the batch once it is full.
func (b *spanBatch) add(s tracing.Span) {
	b.mu.Lock()
	b.spans = append(b.spans, s)
	var full []tracing.Span
	if len(b.spans) >= spanBatchSize {
		full, b.spans = b.spans, nil
	}
	b.mu.Unlock()
	if full != nil {
		b.ev.Spans(full)
	}
}

// flush sends the spans gathered and not yet sent.
func (b *spanBatch) flush() {
	b.mu.Lock()
	rest := b.spans
	b.spans = nil
	b.mu.Unlock()
	if len(rest) > 0 {
		b.ev.Spans(rest)
	}
}
