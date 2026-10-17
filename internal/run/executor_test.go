package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	"go.podman.io/storage"
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

// TestContentStoreWithoutContainerStore checks that the executor's content store opens, and takes content,
// without the container store, which capturing sources doesn't need and an unprivileged host may not open.
func TestContentStoreWithoutContainerStore(t *testing.T) {
	dir := t.TempDir()
	options := storage.StoreOptions{
		GraphRoot:       filepath.Join(dir, "storage"),
		RunRoot:         filepath.Join(dir, "run"),
		GraphDriverName: "no-such-driver", // which would fail to open
	}
	l, err := NewLocal(options)
	testenv.Must(t, err)
	defer l.Close()
	content, err := l.ContentStore()
	testenv.Must(t, err)
	if _, err := content.PutBytes(t.Context(), []byte("source")); err != nil {
		t.Fatal(err)
	}
	if l.store != nil {
		t.Error("opening the content store opened the container store")
	}
	if err := l.open(); err == nil {
		t.Error("opening a container store with no such driver succeeded; the test proves nothing")
	}
}

// awaitJob submits req to l, watches it to its end and returns its outcome.
func awaitJob(t *testing.T, l *Local, req api.Request) api.Outcome {
	t.Helper()
	ctx := t.Context()
	id, err := l.Submit(ctx, req)
	testenv.Must(t, err)
	testenv.Must(t, l.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	outcome, err := l.Result(ctx, id)
	testenv.Must(t, err)
	return outcome
}

// TestExportFilesWithoutContainerStore checks that exporting a task's files, which are in the content store already,
// doesn't open the container store, which it doesn't need and an unprivileged host may not open.
func TestExportFilesWithoutContainerStore(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocal(storage.StoreOptions{
		GraphRoot:       filepath.Join(dir, "storage"),
		RunRoot:         filepath.Join(dir, "run"),
		GraphDriverName: "no-such-driver", // which would fail to open
	})
	testenv.Must(t, err)
	defer l.Close()
	content, err := l.ContentStore()
	testenv.Must(t, err)
	file, err := content.PutBytes(t.Context(), []byte("exported"))
	testenv.Must(t, err)
	tree, err := content.PutBytes(t.Context(), (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)

	outcome := awaitJob(t, l, api.Request{Export: &api.ExportRequest{Result: api.Result{Files: &tree, Epoch: 1}}})
	if outcome.Failure != nil || outcome.Exported == nil || *outcome.Exported.Files != tree ||
		outcome.Exported.Epoch != 1 {
		t.Errorf("exporting a task's files = %+v, want them exported", outcome)
	}
	if l.store != nil {
		t.Error("exporting a task's files opened the container store")
	}
}

// TestExportRepairsCorruptBlob checks that exporting an image publishes every blob again,
// so that one the content store holds corrupt, at the right size, is replaced by what the container store holds.
func TestExportRepairsCorruptBlob(t *testing.T) {
	testenv.RequireBuildah(t)
	ctx := t.Context()
	stores := testenv.PrivateStores(t)
	l, err := NewLocal(stores.Options)
	testenv.Must(t, err)
	defer l.Close()
	content, err := l.ContentStore()
	testenv.Must(t, err)

	// A layer on scratch that adds one file.
	file, err := content.PutBytes(ctx, []byte("repaired"))
	testenv.Must(t, err)
	tree, err := content.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	from := graph.Ref(0)
	platform := platforms.Format(HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: graph.Scratch, Platform: platform,
			Steps: []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}}}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := content.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	built := awaitJob(t, l, api.Request{Graph: &api.GraphRequest{
		Graph: stored, Version: graph.Version, Platform: HostPlatform,
	}})
	if built.Failure != nil {
		t.Fatalf("building = %+v", built.Failure)
	}
	exportReq := api.Request{Export: &api.ExportRequest{Result: built.Results[0]}}
	exported := awaitJob(t, l, exportReq).Exported
	if exported == nil || len(exported.Blobs) == 0 {
		t.Fatalf("exported = %+v, want the image's blobs", exported)
	}

	// Corrupt each exported blob in place, keeping its size, as a bad disk might.
	blobs := append([]cache.Blob{exported.Manifest}, exported.Blobs...)
	for _, b := range blobs {
		path := filepath.Join(CacheDir(stores.Options), "cas", b.Digest.Algorithm().String(), b.Digest.Encoded())
		testenv.Must(t, os.Chmod(path, 0o600))
		testenv.Must(t, os.WriteFile(path, bytes.Repeat([]byte{'!'}, int(b.Size)), 0o600))
		if _, err := content.Read(ctx, b, b.Size); !errors.Is(err, cache.ErrCorrupt) {
			t.Fatalf("reading blob %s after corrupting it = %v, want %v", b.Digest, err, cache.ErrCorrupt)
		}
	}

	again := awaitJob(t, l, exportReq)
	if again.Failure != nil || !reflect.DeepEqual(again.Exported, exported) {
		t.Fatalf("exporting again = %+v, want %+v", again, exported)
	}
	for _, b := range blobs {
		if _, err := content.Read(ctx, b, b.Size); err != nil {
			t.Errorf("blob %s after exporting again: %v", b.Digest, err)
		}
	}
}

// TestCloseCancelsRunningJobs checks that Close cancels a job still running and waits for it to end,
// so that its cleanup has run before Close returns, and that the executor accepts no job afterwards.
func TestCloseCancelsRunningJobs(t *testing.T) {
	l, err := NewLocal(storage.StoreOptions{GraphRoot: filepath.Join(t.TempDir(), "storage")})
	testenv.Must(t, err)
	started, cleaned := make(chan struct{}), false
	id, err := l.jobs.start(false, func(ctx context.Context, _ Events) api.Outcome {
		close(started)
		<-ctx.Done()
		cleaned = true // as removing its container would be
		return failed(ctx.Err())
	})
	testenv.Must(t, err)
	<-started
	testenv.Must(t, l.Close())
	if !cleaned {
		t.Error("Close returned before the running job had ended")
	}
	if outcome, err := l.Result(t.Context(), id); err != nil || !outcome.Cancelled {
		t.Errorf("the job's outcome after Close = %+v, %v; want it cancelled", outcome, err)
	}
	if _, err := l.Prune(t.Context()); !errors.Is(err, errClosed) {
		t.Errorf("a prune after Close = %v, want %v", err, errClosed)
	}
}

// TestJobsRefusedDuringPrune checks that no job starts while an exclusive one, a prune, runs, nor a prune while
// another job runs, and that both are accepted again once it has ended; and that none starts after stop.
func TestJobsRefusedDuringPrune(t *testing.T) {
	var jobs jobTable
	release := make(chan struct{})
	block := func(context.Context, Events) api.Outcome {
		<-release
		return api.Outcome{}
	}
	id, err := jobs.start(true, block)
	testenv.Must(t, err)
	if _, err := jobs.start(false, block); !errors.Is(err, errPruning) {
		t.Errorf("a job during a prune = %v, want %v", err, errPruning)
	}
	if _, err := jobs.start(true, block); !errors.Is(err, errPruning) {
		t.Errorf("a prune during a prune = %v, want %v", err, errPruning)
	}
	close(release)
	j, err := jobs.lookup(id)
	testenv.Must(t, err)
	testenv.Must(t, j.watch(t.Context(), 0, func(api.Event) error { return nil }))

	running := make(chan struct{})
	_, err = jobs.start(false, func(ctx context.Context, _ Events) api.Outcome {
		<-ctx.Done()
		<-running
		return api.Outcome{}
	})
	testenv.Must(t, err)
	if _, err := jobs.start(true, block); err == nil || !strings.Contains(err.Error(), "1 other jobs are running") {
		t.Errorf("a prune while a job runs = %v, want it refused, counting the job", err)
	}
	close(running)
	jobs.stop()
	if _, err := jobs.start(false, block); !errors.Is(err, errClosed) {
		t.Errorf("a job after stop = %v, want %v", err, errClosed)
	}
}
