package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/worker"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/afero"
	"go.podman.io/storage"
)

// The constants of testenv, as this package's tests name them.
const (
	pullBusybox = testenv.PullBusybox
	projectDir  = testenv.ProjectDir
)

// The helpers of testenv, as this package's tests name them.
var (
	project        = testenv.Project
	diskProject    = testenv.DiskProject
	requireBuildah = testenv.RequireBuildah
	must           = testenv.Must
	newNonce       = testenv.NewNonce
)

func TestMain(m *testing.M) {
	testenv.Main(m, worker.ChildFromEnv)
}

// privateStores is the stores of the test that asked for its own, while it runs; see usePrivateStores.
var privateStores *testenv.Stores

// stores returns the stores that vamos runs against: the test's own, if it asked for them, or else the suite's.
func stores(t *testing.T) *testenv.Stores {
	t.Helper()
	if privateStores != nil {
		return privateStores
	}
	return testenv.SuiteStores(t)
}

// usePrivateStores gives the test a container store and build cache of its own, for the rest of the test,
// for a test that destroys what they hold, as prune does.
// Tests run one at a time, so the stores are in a variable; a parallel test can't use it.
func usePrivateStores(t *testing.T) {
	t.Helper()
	t.Setenv("VAMOS_TEST_PRIVATE_STORES", "1") // which fails a parallel test, since the variable is shared
	previous := privateStores
	privateStores = testenv.PrivateStores(t)
	t.Cleanup(func() { privateStores = previous })
}

// insecureRegistry serves a registry over plain HTTP, through handler, which defaults to an empty registry,
// and marks it insecure in a registries.conf of the test's own, so that the executor,
// which takes its registry settings from its environment, reaches it; the client needs --tls-verify=false.
// It returns the server and the registry's host.
func insecureRegistry(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	if handler == nil {
		handler = registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	testenv.InsecureRegistry(t, host)
	return server, host
}

// withStore calls use with the test's container store, open only until use returns.
func withStore(t *testing.T, use func(storage.Store)) {
	t.Helper()
	must(t, stores(t).WithStore(func(store storage.Store) error {
		use(store)
		return nil
	}))
}

// result is the outcome of one vamos invocation.
type result struct {
	stdout, stderr string
	err            error
}

// vamos runs the command line with args against fsys, as main does.
func vamos(t *testing.T, fsys afero.Fs, args ...string) result {
	t.Helper()
	return vamosWithOutput(t, fsys, t.Context(), nil, args...)
}

// vamosWithOutput is like vamos, but runs with ctx, as main does to handle Ctrl-C,
// and also copies stdout to watch, if it isn't nil.
func vamosWithOutput(t *testing.T, fsys afero.Fs, ctx context.Context, watch io.Writer, args ...string) result {
	t.Helper()
	cmd := NewRootCommand(Config{Fs: fsys, Store: &stores(t).Options})
	var stdout, stderr bytes.Buffer
	if watch != nil {
		cmd.SetOut(io.MultiWriter(&stdout, watch))
	} else {
		cmd.SetOut(&stdout)
	}
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return result{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// succeeds fails the test unless r succeeded.
func (r result) succeeds(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("vamos failed: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, " built! ") {
		t.Errorf("stdout = %q, want it to report the image built", r.stdout)
	}
	return r
}

// ran fails the test unless r, a command that sends no image, such as build or prune, succeeded.
func (r result) ran(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("vamos failed: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
	}
	return r
}

// failsWith fails the test unless r failed with an error containing each of wants.
// Usage is expected only for mistakes on the command line.
func (r result) failsWith(t *testing.T, usage bool, wants ...string) result {
	t.Helper()
	if r.err == nil {
		t.Fatalf("vamos succeeded, want error containing %q\nstdout:\n%s", wants, r.stdout)
	}
	for _, want := range wants {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", r.err, want)
		}
	}
	// Cobra prints usage to the output stream if one is set, as in tests, otherwise stderr.
	if got := strings.Contains(r.stdout+r.stderr, "Usage:"); got != usage {
		t.Errorf("printed usage = %v, want %v; stdout:\n%s\nstderr:\n%s", got, usage, r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout, " built! ") {
		t.Errorf("stdout = %q, want no image reported after an error", r.stdout)
	}
	return r
}

// imageCmd returns the default command recorded in img's config.
func imageCmd(t *testing.T, img v1.Image) []string {
	t.Helper()
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	return config.Config.Cmd
}

// imageFile returns the content of the file at path in img's filesystem.
func imageFile(t *testing.T, img v1.Image, path string) string {
	t.Helper()
	var content string
	found := walkImage(t, img, func(hdr *tar.Header, r io.Reader) bool {
		if imagePath(hdr) != path {
			return false
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		content = string(b)
		return true
	})
	if !found {
		t.Fatalf("%s not found in image", path)
	}
	return content
}

// walkImage calls visit with each entry in img's flattened filesystem, until it returns true.
// It reports whether visit did so.
func walkImage(t *testing.T, img v1.Image, visit func(*tar.Header, io.Reader) bool) bool {
	t.Helper()
	found, err := testenv.WalkImage(img, visit)
	must(t, err)
	return found
}

// imagePath returns the absolute path within the image of a layer entry.
func imagePath(hdr *tar.Header) string {
	return testenv.ImagePath(hdr.Name)
}
