package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// TestHandlerWithoutRoot checks that a command handler runs without the root command: the graph command,
// given its options and writers alone, prints the graph.
func TestHandlerWithoutRoot(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": pullBusybox + `layer(name="app", base="busybox")`})
	var stdout, stderr bytes.Buffer
	inv := &invocation{
		app:    &app{fs: fsys, store: &stores(t).Options},
		ctx:    t.Context(),
		stdout: &stdout,
		stderr: &stderr,
		opts:   options{dir: projectDir},
	}
	must(t, inv.graph([]string{"app"}))
	g, err := graph.Decode(stdout.Bytes())
	must(t, err)
	if len(g.Nodes) != 2 || g.Nodes[1].Layer == nil {
		t.Errorf("the graph printed has nodes %+v, want the pull and the layer", g.Nodes)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// TestOutputWriteFailure checks that a failure to write a command's output is the command's error.
func TestOutputWriteFailure(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": pullBusybox + `layer(name="app", base="busybox")`})
	broken := errors.New("stdout is closed")
	inv := &invocation{
		app:    &app{fs: fsys, store: &stores(t).Options},
		ctx:    t.Context(),
		stdout: writerFunc(func([]byte) (int, error) { return 0, broken }),
		stderr: &bytes.Buffer{},
		opts:   options{dir: projectDir},
	}
	if err := inv.graph([]string{"app"}); !errors.Is(err, broken) {
		t.Errorf("graph = %v, want %v", err, broken)
	}
	r := vamosWithOutput(t, fsys, t.Context(), nil, "graph", "app", "-C", projectDir)
	if r.err != nil || !strings.Contains(r.stdout, `"nodes"`) {
		t.Errorf("through the root command, graph = %v, stdout %q", r.err, r.stdout)
	}
}

// TestPruneHandlerWithoutRoot checks that the prune handler runs without the root command, on stores of its own,
// and that a failure to write its report is its error, with the prune still done.
func TestPruneHandlerWithoutRoot(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	var stdout bytes.Buffer
	inv := &invocation{
		app:    &app{fs: project(t, nil), store: &stores(t).Options},
		ctx:    t.Context(),
		stdout: &stdout,
		stderr: &bytes.Buffer{},
		opts:   defaultOptions(),
	}
	must(t, inv.prune())
	if !strings.HasPrefix(stdout.String(), "Removed 0 images") {
		t.Errorf("prune printed %q", stdout.String())
	}
	broken := errors.New("stdout is closed")
	inv.stdout = writerFunc(func([]byte) (int, error) { return 0, broken })
	if err := inv.prune(); !errors.Is(err, broken) {
		t.Errorf("prune with a broken stdout = %v, want %v", err, broken)
	}
}

// TestBuildHandlerWithoutRoot checks that the build handler, with a trace, runs without the root command,
// and that a failure to write the build's progress stops the build and is reported.
func TestBuildHandlerWithoutRoot(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": `glob(name="src", include=["a.txt"])` + "\n" +
			`layer(name="app", base="scratch", steps=[add("src", "/data")])`,
		"a.txt": newNonce(),
	})
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	var stdout bytes.Buffer
	opts := defaultOptions()
	opts.dir, opts.trace = projectDir, tracePath
	inv := &invocation{
		app:    &app{fs: fsys, store: &stores(t).Options},
		ctx:    t.Context(),
		stdout: &stdout,
		stderr: &bytes.Buffer{},
		opts:   opts,
	}
	must(t, inv.build([]string{"app"}))
	if !strings.Contains(stdout.String(), `layer "app": building`) {
		t.Errorf("build printed %q", stdout.String())
	}
	if _, err := os.Stat(tracePath); err != nil {
		t.Errorf("no trace written: %v", err)
	}

	broken := errors.New("stdout is closed")
	inv.stdout = writerFunc(func([]byte) (int, error) { return 0, broken })
	inv.opts.trace, inv.opts.rebuild = "", true
	err := inv.build([]string{"app"})
	if !errors.Is(err, broken) || !strings.Contains(err.Error(), "writing the build's progress") {
		t.Errorf("build with a broken stdout = %v, want the write's failure, named", err)
	}
}

// drainingExecutor is a job whose events, watched, are a status line then, once the job is cancelled,
// the executor's spans, a gap and its end, with outcome as its result.
type drainingExecutor struct {
	api.Executor
	outcome   api.Outcome
	cancelled bool
}

func (x *drainingExecutor) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	events := []api.Event{{Position: 0, Kind: api.EventStatus, Nodes: []graph.Ref{0}, Status: "building"}}
	if x.cancelled {
		events = append(events,
			api.Event{Position: 1, Kind: api.EventSpans, Spans: []tracing.Span{{ID: "s", Name: "step"}}},
			api.Event{Position: 2, Kind: api.EventGap, First: 4},
			api.Event{Position: 4, Kind: api.EventDone, Outcome: &x.outcome})
	}
	for _, e := range events {
		if e.Position < from {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func (x *drainingExecutor) Cancel(context.Context, api.JobID) error {
	x.cancelled = true
	return nil
}

func (x *drainingExecutor) Result(context.Context, api.JobID) (api.Outcome, error) {
	return x.outcome, nil
}

// TestWatchJobWriteFailure checks that a failure to write a job's events cancels the job,
// that its spans and gaps still reach the trace while it is drained, and that a failure of the job's own
// is reported with the write's, and otherwise the write's failure alone.
func TestWatchJobWriteFailure(t *testing.T) {
	broken := errors.New("stdout is closed")
	for _, tc := range []struct {
		name    string
		outcome api.Outcome
		want    string // in the error, besides the write's failure
	}{
		{"cancelled", api.Outcome{Cancelled: true, Failure: &api.Failure{Message: "cancelled"}}, ""},
		{"failed", api.Outcome{Failure: &api.Failure{Message: "the step failed"}}, "the step failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &drainingExecutor{outcome: tc.outcome}
			trace := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
			events := &eventPrinter{
				stdout: writerFunc(func([]byte) (int, error) { return 0, broken }),
				stderr: &bytes.Buffer{},
				trace:  trace,
			}
			_, _, err := watchJob(t.Context(), x, "job", events)
			if !errors.Is(err, broken) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("watchJob = %v, want the write's failure and %q", err, tc.want)
			}
			if !x.cancelled {
				t.Error("the job wasn't cancelled")
			}
			if len(trace.remote) != 1 {
				t.Errorf("the trace has executor spans %+v, want the one sent while draining", trace.remote)
			}
			gap := false
			for _, s := range trace.recorder.Spans() {
				gap = gap || s.Name == "trace incomplete"
			}
			if !gap {
				t.Error("the trace doesn't say it is incomplete")
			}
		})
	}
}
