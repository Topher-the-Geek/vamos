package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.podman.io/storage"
)

// helloBuild copies a source file into busybox and sets a command that echoes.
var helloBuild = map[string]string{
	"BUILD": pullBusybox + `
glob(name="hello", include=["hello.txt"])
layer(
    name="app",
    base="busybox",
    steps=[add("hello", "/data")],
    command=["echo", "hello from vamos"],
)
`,
	"hello.txt": "hello, file\n",
}

func TestInterrupted(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox +
			`layer(name="app", base="busybox", steps=[run(["sh", "-c", "echo started; sleep 1"])])`,
	})
	out := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Interrupt, as main does on Ctrl-C, once the run() step has started.
	watch := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "started") {
			cancel()
		}
		return len(p), nil
	})

	vamosWithOutput(t, fsys, ctx, watch, "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "context canceled")
	assertNoFiles(t, out)
	// The cancellation was reported only once the step's container was removed.
	withStore(t, func(store storage.Store) {
		containers, err := store.Containers()
		must(t, err)
		if len(containers) != 0 {
			t.Errorf("%d containers left in the store after the interrupted build", len(containers))
		}
	})
}

// writerFunc is an io.Writer that calls itself.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
