package run

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// spanEvents collects the batches of spans sent to it.
type spanEvents struct {
	mu      sync.Mutex
	batches [][]tracing.Span
}

func (e *spanEvents) Status(graph.Ref, string)       {}
func (e *spanEvents) Output(graph.Ref, byte, []byte) {}
func (e *spanEvents) Spans(spans []tracing.Span) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.batches = append(e.batches, spans)
}

// TestSpanBatches checks that the spans of a call, ended from several goroutines at once,
// each reach its events exactly once, in batches no larger than spanBatchSize, the rest when the call flushes them,
// and none before it unless a batch is full.
func TestSpanBatches(t *testing.T) {
	const goroutines, each = 8, 100 // more than spanBatchSize in all, but not a whole number of batches
	ev := &spanEvents{}
	ctx, flush := forCall(context.Background(), &tracing.CallContext{Parent: "client-1"}, ev)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				_, span := tracing.Start(ctx, fmt.Sprintf("span %d.%d", g, i))
				span.End()
			}
		})
	}
	wg.Wait()
	ev.mu.Lock()
	before := len(ev.batches)
	ev.mu.Unlock()
	if want := goroutines * each / spanBatchSize; before != want {
		t.Errorf("%d batches sent before flushing, want %d full ones", before, want)
	}
	flush()

	seen := map[string]bool{}
	for i, batch := range ev.batches {
		if len(batch) > spanBatchSize || i < before && len(batch) != spanBatchSize {
			t.Errorf("batch %d has %d spans, want %d", i, len(batch), spanBatchSize)
		}
		for _, s := range batch {
			if seen[s.Name] {
				t.Errorf("%s was sent twice", s.Name)
			}
			seen[s.Name] = true
		}
	}
	if len(seen) != goroutines*each {
		t.Errorf("%d spans sent, want %d", len(seen), goroutines*each)
	}
	flush()
	if len(ev.batches) != before+1 {
		t.Errorf("flushing again sent %d more batches, want none", len(ev.batches)-before-1)
	}
}

// TestSpanBatchesUntraced checks that a call that isn't traced sends no spans.
func TestSpanBatchesUntraced(t *testing.T) {
	ev := &spanEvents{}
	ctx, flush := forCall(context.Background(), nil, ev)
	_, span := tracing.Start(ctx, "untraced")
	span.End()
	flush()
	if len(ev.batches) != 0 {
		t.Errorf("an untraced call sent %d batches", len(ev.batches))
	}
}
