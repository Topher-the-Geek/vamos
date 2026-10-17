package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestDisabled checks that spans started without a recorder do nothing, and that their methods are safe to call.
func TestDisabled(t *testing.T) {
	ctx := context.Background()
	got, s := Start(ctx, "step", "files", 3)
	if got != ctx || s != nil {
		t.Errorf("Start without a recorder = %v, %v; want ctx and nil", got, s)
	}
	s.Set("bytes", 4)
	s.Produce("x")
	s.EndWith(errors.New("failed"))
	StartAsync(ctx, "blob").End()
	Instant(ctx, "skipped")
	if Call(ctx) != nil || Enabled(ctx) {
		t.Error("a context without a recorder asks for tracing")
	}
}

// TestSpans checks that spans record their parents, tracks and arguments,
// that only the first End counts, and that a recorder that streams spans doesn't also keep them.
func TestSpans(t *testing.T) {
	var streamed []Span
	r := NewRecorder("executor", func(s Span) { streamed = append(streamed, s) })
	ctx := WithRecorder(context.Background(), r, "client-1")

	ctx, run := StartTrack(ctx, "run", "run")
	tctx, layer := StartTrack(ctx, `layer "app"`, `layer "app"`, "key", "sha256:abc", "missing", nil)
	_, step := Start(tctx, `run(["true"])`)
	step.Set("stdout bytes", 0)
	step.End()
	blob := StartAsync(tctx, "blob sha256:def")
	blob.EndWith(errors.New("cancelled"))
	Instant(tctx, "skipped sha256:123")
	layer.End()
	layer.Set("late", true) // after End, which has recorded the span already
	layer.End()
	run.End()

	if kept := r.Spans(); len(kept) != 0 || len(streamed) != 5 {
		t.Fatalf("kept %d spans and streamed %d, want none kept and 5 streamed", len(kept), len(streamed))
	}
	byName := map[string]Span{}
	for _, s := range streamed {
		byName[s.Name] = s
		if s.Process != "executor" {
			t.Errorf("%s: process = %q, want executor", s.Name, s.Process)
		}
	}
	for _, tt := range []struct {
		name   string
		parent string
		track  string
	}{
		{
			name:   "run",
			parent: "client-1",
			track:  "run",
		},
		{
			name:   `layer "app"`,
			parent: run.ID(),
			track:  `layer "app"`,
		},
		{
			name:   `run(["true"])`,
			parent: layer.ID(),
			track:  `layer "app"`,
		},
		{
			name:   "blob sha256:def",
			parent: layer.ID(),
			track:  `layer "app"`,
		},
		{
			name:   "skipped sha256:123",
			parent: layer.ID(),
			track:  `layer "app"`,
		},
	} {
		s := byName[tt.name]
		if s.Parent != tt.parent || s.Track != tt.track {
			t.Errorf("%s: parent %q on track %q, want %q on %q", tt.name, s.Parent, s.Track, tt.parent, tt.track)
		}
	}
	l := byName[`layer "app"`]
	if _, ok := l.Args["missing"]; ok {
		t.Error("a nil argument was recorded")
	}
	if _, ok := l.Args["late"]; ok {
		t.Error("an argument set after End was recorded")
	}
	if got := byName["blob sha256:def"]; !got.Async || got.Args["error"] != "cancelled" {
		t.Errorf("blob = %+v, want an async span ended with its error", got)
	}
	if got := byName["skipped sha256:123"]; !got.Instant || !got.End.Equal(got.Start) {
		t.Errorf("skipped = %+v, want an instant", got)
	}
}

// TestCall checks that a call's context carries the span it is made within to the executor's recorder.
func TestCall(t *testing.T) {
	ctx := WithRecorder(context.Background(), NewRecorder("vamos", nil), "")
	ctx, finish := Start(ctx, "finish")
	call := Call(ctx)
	if call == nil || call.Parent != finish.ID() {
		t.Fatalf("Call = %+v, want the parent %q", call, finish.ID())
	}
	r := NewRecorder("executor", nil)
	_, push := StartTrack(WithRecorder(context.Background(), r, call.Parent), "push", "push")
	push.End()
	if got := r.Spans()[0].Parent; got != finish.ID() {
		t.Errorf("the executor's span's parent = %q, want the client's %q", got, finish.ID())
	}
	if ForCall(context.Background(), nil, nil, "executor") != context.Background() {
		t.Error("ForCall with no call recorded spans")
	}
}

// TestEstimateOffset checks that an executor's clock that is off is corrected for,
// even when its spans would still fall within the client's call to it,
// which a check of containment alone wouldn't catch.
func TestEstimateOffset(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	for _, tt := range []struct {
		name   string
		skew   time.Duration
		delays []time.Duration // each call's time to reach the executor, and the same back
	}{
		{
			name:   "ahead",
			skew:   2 * time.Second,
			delays: []time.Duration{40 * time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond},
		},
		{
			name:   "behind",
			skew:   -3 * time.Second,
			delays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		},
		{
			name:   "same clock",
			skew:   0,
			delays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock := base
			now := func() time.Time { return clock }
			call := 0
			remote := func(context.Context) (time.Time, error) {
				delay := tt.delays[call]
				call++
				clock = clock.Add(delay)
				answer := clock.Add(tt.skew)
				clock = clock.Add(delay)
				return answer, nil
			}
			got, err := EstimateOffset(context.Background(), now, remote)
			if err != nil {
				t.Fatal(err)
			}
			if diff := (got.Offset - tt.skew).Abs(); diff > got.Uncertainty {
				t.Errorf("offset = %v ± %v, want %v", got.Offset, got.Uncertainty, tt.skew)
			}
			if want := 1 * time.Millisecond; tt.skew == 0 && got.Uncertainty != want {
				t.Errorf("uncertainty = %v, want %v", got.Uncertainty, want)
			}

			// The client's 10s call holds the executor's 1s run, by the executor's clock, despite the skew.
			callStart := base.Add(time.Minute)
			callEnd := callStart.Add(10 * time.Second)
			runStart := callStart.Add(4 * time.Second)
			spans := Shift([]Span{{Start: runStart.Add(tt.skew), End: runStart.Add(tt.skew + time.Second)}}, got)
			if !spans[0].Start.Equal(runStart) || spans[0].End.Before(spans[0].Start) {
				t.Errorf("shifted run starts at %v, want %v", spans[0].Start.Sub(callStart), runStart.Sub(callStart))
			}
			if spans[0].Start.Before(callStart) || spans[0].End.After(callEnd) {
				t.Error("the shifted run isn't within the call")
			}
		})
	}
}

// TestEstimateOffsetFails checks that a failing call to the executor is reported.
func TestEstimateOffsetFails(t *testing.T) {
	_, err := EstimateOffset(context.Background(), time.Now, func(context.Context) (time.Time, error) {
		return time.Time{}, errors.New("unreachable")
	})
	if err == nil || err.Error() != "unreachable" {
		t.Errorf("err = %v, want unreachable", err)
	}
}

// TestWriteChrome checks the events written: nested slices on their tracks, async spans as begin and end, instants,
// and arrows from what produced something to what used it.
func TestWriteChrome(t *testing.T) {
	at := func(ms int) time.Time { return time.Unix(1_800_000_000, 0).Add(time.Duration(ms) * time.Millisecond) }
	spans := []Span{
		{ID: "c-2", Parent: "c-1", Process: "vamos", Track: "main", Name: "glob", Start: at(1), End: at(2),
			Produces: "node 0", Args: map[string]any{"files": 3}},
		{ID: "c-1", Process: "vamos", Track: "main", Name: "build", Start: at(0), End: at(10)},
		{ID: "e-1", Parent: "c-1", Process: "executor", Track: "run", Name: "run", Start: at(3), End: at(9)},
		{ID: "e-3", Parent: "e-2", Process: "executor", Track: "layer", Name: "add", Start: at(4), End: at(5),
			Uses: "node 0"},
		{ID: "e-4", Parent: "e-2", Process: "executor", Track: "layer", Name: "blob", Start: at(5), End: at(7),
			Async: true},
		{ID: "e-5", Parent: "e-2", Process: "executor", Track: "layer", Name: "skipped", Start: at(6), End: at(6),
			Instant: true},
		{ID: "e-2", Parent: "e-1", Process: "executor", Track: "layer", Name: "layer", Start: at(4), End: at(8)},
	}
	var buf bytes.Buffer
	if err := WriteChrome(&buf, spans); err != nil {
		t.Fatal(err)
	}
	var trace struct {
		TraceEvents []chromeEvent `json:"traceEvents"`
	}
	if err := json.Unmarshal(buf.Bytes(), &trace); err != nil {
		t.Fatalf("%v in %s", err, buf.Bytes())
	}
	var order []string
	names := map[[2]int]string{}
	byName := map[string]chromeEvent{}
	for _, e := range trace.TraceEvents {
		switch e.Phase {
		case "M":
			if e.Name == "thread_name" {
				names[[2]int{e.PID, e.TID}] = e.Args["name"].(string)
			}
			continue
		case "e":
			byName["end "+e.Name] = e
		default:
			byName[e.Name] = e
		}
		order = append(order, e.Phase+" "+e.Name)
	}
	want := []string{"X build", "X glob", "X run", "X layer", "X add", "b blob", "e blob", "i skipped"}
	if len(order) != len(want) {
		t.Fatalf("events = %q, want %q", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("events = %q, want %q", order, want)
		}
	}
	build, glob, layer, add := byName["build"], byName["glob"], byName["layer"], byName["add"]
	if build.PID == byName["run"].PID {
		t.Error("the client and the executor are the same process")
	}
	if glob.TID != build.TID || layer.TID != add.TID || layer.TID == byName["run"].TID {
		t.Error("spans aren't on their tracks")
	}
	if got := names[[2]int{layer.PID, layer.TID}]; got != "layer" {
		t.Errorf("the layer's track is named %q", got)
	}
	if *build.TS != 0 || *layer.TS != 4000 || *layer.Dur != 4000 {
		t.Errorf("build at %v, layer at %v for %v; want 0, and 4000 for 4000", *build.TS, *layer.TS, *layer.Dur)
	}
	if !glob.FlowOut || !add.FlowIn || glob.BindID == "" || glob.BindID != add.BindID {
		t.Errorf("no arrow from glob (%+v) to add (%+v)", glob, add)
	}
	if blob, end := byName["blob"], byName["end blob"]; blob.ID != end.ID || blob.Cat != end.Cat {
		t.Errorf("the blob's begin %+v and end %+v don't pair", blob, end)
	}
	if glob.Args["files"] != float64(3) {
		t.Errorf("glob's args = %v", glob.Args)
	}
}
