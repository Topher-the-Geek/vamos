package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Span is one thing that a process did, from its start to its end, or at an instant.
type Span struct {
	ID      string         `json:"id"`
	Parent  string         `json:"parent,omitempty"`  // the span it is part of, perhaps in another process
	Process string         `json:"process"`           // as the trace names it, such as "vamos" or "executor"
	Track   string         `json:"track"`             // spans on one track nest; a span starts its own if it may not
	Name    string         `json:"name"`              // such as `layer "app"` or `run(["make"])`
	Start   time.Time      `json:"start"`             // by the process's clock
	End     time.Time      `json:"end"`               // the same as Start for an instant
	Async   bool           `json:"async,omitempty"`   // it may overlap others on its track, as blobs copied at once do
	Instant bool           `json:"instant,omitempty"` // it took no time, as a blob skipped does
	Args    map[string]any `json:"args,omitempty"`    // what it read, wrote and made
	// Produces or Uses names what a span made or needed,
	// so that the trace can draw an arrow from the span that made something to each that used it,
	// even in another process.
	// Perfetto binds a slice to one arrow's end alone, so a span has at most one of them.
	Produces string `json:"produces,omitempty"`
	Uses     string `json:"uses,omitempty"`
}

// CallContext asks an executor to trace a call, as a part of the client's span Parent.
type CallContext struct {
	Parent string `json:"parent"`
}

// Recorder collects the spans of one process, or of one call to an executor, as they end.
type Recorder struct {
	process string
	prefix  string // of its spans' IDs, so that no two recorders' collide
	onEnd   func(Span)

	mu    sync.Mutex
	next  int
	spans []Span
}

// NewRecorder returns a recorder of the spans of process.
// If onEnd isn't nil, it passes each span to onEnd as it ends instead of keeping it.
// onEnd may be called from several goroutines at once.
// The callback runs outside recorder and Active locks, and must not mutate the span's Args map.
func NewRecorder(process string, onEnd func(Span)) *Recorder {
	var b [4]byte
	rand.Read(b[:])
	return &Recorder{process: process, prefix: hex.EncodeToString(b[:]), onEnd: onEnd}
}

// Spans returns the spans that have ended, in the order they ended, or none if the recorder passes them to onEnd.
// The slice is copied, but Args maps are shared and must be treated as read-only.
func (r *Recorder) Spans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Span(nil), r.spans...)
}

// newID returns an ID that no other span has.
func (r *Recorder) newID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return fmt.Sprintf("%s-%d", r.prefix, r.next)
}

// add records the ended span s.
func (r *Recorder) add(s Span) {
	if r.onEnd != nil {
		r.onEnd(s) // which keeps it, so that the recorder needn't hold a copy no one reads
		return
	}
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
}

// recording is what a context says about tracing: where spans go, and the span that new ones are part of.
type recording struct {
	recorder *Recorder
	parent   string  // a span's ID, perhaps in another process
	span     *Active // the parent, if it is in this process
	track    string
}

type contextKey struct{}

// WithRecorder returns a context whose spans r records, with spans started from it part of parent,
// which may be a span in another process, or "".
func WithRecorder(ctx context.Context, r *Recorder, parent string) context.Context {
	return context.WithValue(ctx, contextKey{}, &recording{recorder: r, parent: parent})
}

// ForCall returns a context whose spans r records, as part of the span call names, or ctx itself if call is nil.
func ForCall(ctx context.Context, call *CallContext, onEnd func(Span), process string) context.Context {
	if call == nil {
		return ctx
	}
	return WithRecorder(ctx, NewRecorder(process, onEnd), call.Parent)
}

// Call returns what asks an executor to trace a call made within ctx's span, or nil if ctx isn't traced.
func Call(ctx context.Context) *CallContext {
	rec, _ := ctx.Value(contextKey{}).(*recording)
	if rec == nil {
		return nil
	}
	return &CallContext{Parent: rec.parent}
}

// Current returns the span that spans started from ctx are part of,
// or nil if there is none in this process, so that what it calls can record what it learns there.
func Current(ctx context.Context) *Active {
	rec, _ := ctx.Value(contextKey{}).(*recording)
	if rec == nil {
		return nil
	}
	return rec.span
}

// Enabled reports whether spans started from ctx are recorded.
func Enabled(ctx context.Context) bool {
	return ctx.Value(contextKey{}) != nil
}

// Active is a span that hasn't ended.
// Its methods may be called from several goroutines.
type Active struct {
	recorder *Recorder
	mu       sync.Mutex
	span     Span
	ended    bool
}

// ID returns the span's ID.
func (a *Active) ID() string {
	if a == nil {
		return ""
	}
	return a.span.ID
}

// Start starts a span named name, on the track of the span it is part of, with args, a list of keys and values.
// It returns a context for spans that are part of it.
// With no recorder in ctx it does nothing, and returns ctx and a nil span, whose methods do nothing.
func Start(ctx context.Context, name string, args ...any) (context.Context, *Active) {
	return start(ctx, "", name, false, args)
}

// StartTrack is like Start, but puts the span on the track named track, of its own, such as for a target,
// which may run at the same time as others.
func StartTrack(ctx context.Context, track, name string, args ...any) (context.Context, *Active) {
	return start(ctx, track, name, false, args)
}

// StartAsync is like Start, but for a span that may overlap others on its track.
func StartAsync(ctx context.Context, name string, args ...any) *Active {
	_, a := start(ctx, "", name, true, args)
	return a
}

// Instant records an event that took no time, as part of ctx's span.
func Instant(ctx context.Context, name string, args ...any) {
	_, a := start(ctx, "", name, false, args)
	if a != nil {
		a.span.Instant = true
		a.end(a.span.Start)
	}
}

// start starts a span, on track if it isn't "".
func start(ctx context.Context, track, name string, async bool, args []any) (context.Context, *Active) {
	rec, _ := ctx.Value(contextKey{}).(*recording)
	if rec == nil {
		return ctx, nil
	}
	if track == "" {
		track = rec.track
	}
	if track == "" {
		track = name
	}
	a := &Active{
		recorder: rec.recorder,
		span: Span{
			ID:      rec.recorder.newID(),
			Parent:  rec.parent,
			Process: rec.recorder.process,
			Track:   track,
			Name:    name,
			Start:   time.Now(),
			Async:   async,
			Args:    map[string]any{},
		},
	}
	a.Set(args...)
	child := &recording{recorder: rec.recorder, parent: a.span.ID, span: a, track: track}
	return context.WithValue(ctx, contextKey{}, child), a
}

// Set records args, a list of keys and values, as what the span read, wrote or made.
// A nil value is left out, and so is anything set once the span has ended.
// Values are retained without a deep copy; callers must not mutate maps or slices stored as arguments.
func (a *Active) Set(args ...any) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return
	}
	for i := 0; i+1 < len(args); i += 2 {
		if args[i+1] != nil {
			a.span.Args[fmt.Sprint(args[i])] = args[i+1]
		}
	}
}

// SetError records err as what ended the span, if it isn't nil.
func (a *Active) SetError(err error) {
	if err != nil {
		a.Set("error", err.Error())
	}
}

// Produce records that the span made what key names, which spans that use it name too.
// A span that uses something can't also produce something.
func (a *Active) Produce(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ended {
		a.span.Produces = key
	}
}

// Use records that the span needed what key names.
// A span that produces something can't also use something.
func (a *Active) Use(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ended {
		a.span.Uses = key
	}
}

// End ends the span now, and records it.
// Only its first End counts.
func (a *Active) End() {
	if a != nil {
		a.end(time.Now())
	}
}

// EndWith records err, if it isn't nil, then ends the span.
func (a *Active) EndWith(err error) {
	a.SetError(err)
	a.End()
}

// end ends the span at t.
func (a *Active) end(t time.Time) {
	a.mu.Lock()
	if a.ended {
		a.mu.Unlock()
		return
	}
	a.ended = true
	a.span.End = t
	ended := a.span
	a.mu.Unlock()
	a.recorder.add(ended)
}
