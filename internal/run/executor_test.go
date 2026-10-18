package run

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/containerd/platforms"
	"go.podman.io/storage"
)

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
	id, err := l.jobs.Start(false, func(ctx context.Context, _ sched.Events) api.Outcome {
		close(started)
		<-ctx.Done()
		cleaned = true // as removing its container would be
		return sched.Failed(ctx.Err())
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
	if _, err := l.Prune(t.Context()); !errors.Is(err, sched.ErrClosed) {
		t.Errorf("a prune after Close = %v, want %v", err, sched.ErrClosed)
	}
}
