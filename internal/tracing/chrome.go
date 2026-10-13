package tracing

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"
)

// chromeEvent is an event of Chrome's JSON trace format, as Perfetto reads it.
type chromeEvent struct {
	Phase   string         `json:"ph"`
	Name    string         `json:"name"`
	Cat     string         `json:"cat,omitempty"`
	ID      string         `json:"id,omitempty"` // of an async span, pairing its begin and end
	PID     int            `json:"pid"`
	TID     int            `json:"tid"`
	TS      *float64       `json:"ts,omitempty"`  // microseconds since the trace's start; unset for metadata
	Dur     *float64       `json:"dur,omitempty"` // microseconds, of a complete slice
	Scope   string         `json:"s,omitempty"`   // of an instant: "t" for its thread
	BindID  string         `json:"bind_id,omitempty"`
	FlowIn  bool           `json:"flow_in,omitempty"`
	FlowOut bool           `json:"flow_out,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
}

// asyncCategory is the category of async spans, which their begin and end events share.
const asyncCategory = "async"

// WriteChrome writes spans to w as a trace in Chrome's JSON format, which Perfetto opens: each process's tracks as
// threads, its spans as slices on them, and arrows from each span that produced something to those that used it.
// Spans on a track that aren't async must nest, as they do when each is part of the one before that is still
// going.
//
// Times are microseconds since the first span's start, with nanoseconds as a fraction, which Perfetto reads
// exactly. Whole microseconds would let a span that starts in its parent's last microsecond escape it, and
// microseconds since 1970 have too many digits for a fraction to survive. The first span's start is given as the
// trace's metadata.
func WriteChrome(w io.Writer, spans []Span) error {
	spans = slices.Clone(spans)
	// By start, and the longer first, so that a span comes before those within it that start at the same time.
	slices.SortStableFunc(spans, func(a, b Span) int {
		return cmp.Or(a.Start.Compare(b.Start), b.End.Compare(a.End))
	})

	var start time.Time
	if len(spans) > 0 {
		start = spans[0].Start
	}
	micros := func(t time.Time) *float64 {
		us := float64(t.Sub(start).Nanoseconds()) / 1000
		return &us
	}

	var events []chromeEvent
	pids := map[string]int{}
	tids := map[[2]string]int{}
	thread := func(s Span) (int, int) {
		pid, ok := pids[s.Process]
		if !ok {
			pid = len(pids) + 1
			pids[s.Process] = pid
			events = append(events, chromeEvent{
				Phase: "M", Name: "process_name", PID: pid, Args: map[string]any{"name": s.Process},
			}, chromeEvent{
				Phase: "M", Name: "process_sort_index", PID: pid, Args: map[string]any{"sort_index": pid},
			})
		}
		key := [2]string{s.Process, s.Track}
		tid, ok := tids[key]
		if !ok {
			tid = len(tids) + 1
			tids[key] = tid
			events = append(events, chromeEvent{
				Phase: "M", Name: "thread_name", PID: pid, TID: tid, Args: map[string]any{"name": s.Track},
			}, chromeEvent{
				Phase: "M", Name: "thread_sort_index", PID: pid, TID: tid, Args: map[string]any{"sort_index": tid},
			})
		}
		return pid, tid
	}
	// Perfetto binds an arrow's ends by IDs it reads as numbers.
	flows := map[string]string{}
	flowID := func(key string) string {
		id, ok := flows[key]
		if !ok {
			id = fmt.Sprintf("0x%x", len(flows)+1)
			flows[key] = id
		}
		return id
	}

	for _, s := range spans {
		pid, tid := thread(s)
		ts, end := micros(s.Start), micros(s.End)
		dur := float64(s.End.Sub(s.Start).Nanoseconds()) / 1000
		args := s.Args
		if len(args) == 0 {
			args = nil
		}
		switch {
		case s.Instant:
			events = append(events, chromeEvent{
				Phase: "i", Name: s.Name, PID: pid, TID: tid, TS: ts, Scope: "t", Args: args,
			})
		case s.Async:
			events = append(events, chromeEvent{
				Phase: "b", Name: s.Name, Cat: asyncCategory, ID: s.ID, PID: pid, TID: tid, TS: ts, Args: args,
			}, chromeEvent{
				Phase: "e", Name: s.Name, Cat: asyncCategory, ID: s.ID, PID: pid, TID: tid, TS: end,
			})
		default:
			e := chromeEvent{Phase: "X", Name: s.Name, PID: pid, TID: tid, TS: ts, Dur: &dur, Args: args}
			if s.Uses != "" {
				e.BindID, e.FlowIn = flowID(s.Uses), true
			} else if s.Produces != "" {
				e.BindID, e.FlowOut = flowID(s.Produces), true
			}
			events = append(events, e)
		}
	}
	if events == nil {
		events = []chromeEvent{}
	}
	trace := map[string]any{
		"traceEvents":     events,
		"displayTimeUnit": "ms",
	}
	if !start.IsZero() {
		trace["metadata"] = map[string]any{"start": start.UTC().Format(time.RFC3339Nano)}
	}
	return json.NewEncoder(w).Encode(trace)
}
