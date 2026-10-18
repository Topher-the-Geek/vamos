package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/load"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
)

// TestCancelLoad checks that interrupting a build while a BUILD file runs stops it promptly,
// even one that would never end, and reports the interruption rather than a mistake in the file.
func TestCancelLoad(t *testing.T) {
	// Starlark has no while loop, and loops only within functions; a range this long runs for days.
	fsys := project(t, map[string]string{"BUILD": pullBusybox +
		"def spin():\n    for i in range(1 << 40):\n        pass\nspin()\n"})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := vamosWithOutput(t, fsys, ctx, nil, "build", "busybox", "-C", projectDir)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Errorf("vamos = %v, want the context's %v", r.err, context.DeadlineExceeded)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the interrupted load took %s", took)
	}
}

// TestCancelConfigure checks that interrupting a target's configuration, a function of ctx that never ends,
// stops it promptly and reports the interruption rather than a mistake in the file.
func TestCancelConfigure(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": pullBusybox +
		"def spin(ctx):\n    for i in range(1 << 40):\n        pass\n" +
		`layer(name="app", base="busybox", steps=spin)` + "\n"})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := vamosWithOutput(t, fsys, ctx, nil, "build", "app", "-C", projectDir)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Errorf("vamos = %v, want the context's %v", r.err, context.DeadlineExceeded)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the interrupted configuration took %s", took)
	}
}

// TestFailedStepLog checks that what a failed step printed is kept as a log blob that the failure names,
// readable after the job has ended.
func TestFailedStepLog(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	f, g, content := failedRun(t, pullBusybox+`layer(name="app", base="busybox", steps=[run(["sh", "-c", "echo oops `+
		nonce+`; exit 3"])])`, "app")
	if f.Step == nil || f.Log == nil {
		t.Fatalf("failure = %+v, want a failure at a step with its log", f)
	}
	if want := `layer "app": run(["sh", "-c", "echo oops ` + nonce + `; exit 3"]) failed: `; !strings.HasPrefix(
		f.Describe(g), want) {
		t.Errorf("failure = %q, want it to start with %q", f.Describe(g), want)
	}
	assertLogHas(t, content, f, "oops "+nonce)
}

// TestFailedOutputLog checks that a task whose steps succeed but whose output can't be collected
// keeps what its steps printed, as the failure's log.
func TestFailedOutputLog(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	f, g, content := failedRun(t, pullBusybox+`task(name="make", base="busybox", steps=[run(["echo", "made `+nonce+
		`"])], source="/missing", include=["."])`, "make")
	if f.Step != nil || f.Log == nil {
		t.Fatalf("failure = %+v, want a failure after the steps with their log", f)
	}
	if want := `task "make": source "/missing" not found`; !strings.Contains(f.Describe(g), want) {
		t.Errorf("failure = %q, want it to contain %q", f.Describe(g), want)
	}
	assertLogHas(t, content, f, "made "+nonce)
}

// failedRun loads target from a project whose BUILD file is build, runs it on a local executor and returns
// its failure, the graph it ran and the executor's content store, which t's cleanup closes.
func failedRun(t *testing.T, build, target string) (*api.Failure, *graph.Graph, cache.ContentStore) {
	t.Helper()
	ctx := t.Context()
	fsys := project(t, map[string]string{"BUILD": build})
	ex, err := run.NewLocal(stores(t).Options)
	testenv.Must(t, err)
	t.Cleanup(func() { ex.Close() })
	content, err := ex.ContentStore()
	testenv.Must(t, err)
	caps, err := ex.Capabilities(ctx)
	testenv.Must(t, err)
	g, err := load.Load(ctx, fsys, load.Request{
		Dir:          projectDir,
		Targets:      []string{target},
		Want:         load.WantBuild,
		ExecPlatform: caps.Platforms[0],
		Store: func(ctx context.Context, fsys afero.Fs, dir string, sel *fileset.Selection) (
			cache.Blob, fileset.Stats, error) {
			return fileset.Store(ctx, content, fsys, dir, sel)
		},
	})
	testenv.Must(t, err)
	stored, err := content.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	id, err := ex.Submit(ctx, api.Request{Graph: &api.GraphRequest{
		Graph: stored, Version: g.Version, Platform: caps.Platforms[0],
	}})
	testenv.Must(t, err)
	testenv.Must(t, ex.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	outcome, err := ex.Result(ctx, id)
	testenv.Must(t, err)
	if outcome.Failure == nil {
		t.Fatalf("outcome = %+v, want a failure", outcome)
	}
	return outcome.Failure, g, content
}

// assertLogHas checks that the log f names, in content, replays output containing want.
func assertLogHas(t *testing.T, content cache.ContentStore, f *api.Failure, want string) {
	t.Helper()
	ctx := t.Context()
	data, err := content.Read(ctx, *f.Log, cache.MaxLogSize)
	testenv.Must(t, err)
	var out bytes.Buffer
	testenv.Must(t, cache.ReplayLog(ctx, bytes.NewReader(data), &out, &out))
	if !strings.Contains(out.String(), want) {
		t.Errorf("the failure's log replays %q, want %q", out.String(), want)
	}
}
