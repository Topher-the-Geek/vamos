package internal

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/storage"
)

// pullBusybox declares the base image for tests, as the step "busybox", in one line.
// Tests that build pull it once, and later runs find it in the local container store.
const pullBusybox = `pull(name="busybox", image="docker.io/library/busybox", tag="1.36")` + "\n"

// projectDir is where in-memory test projects live.
const projectDir = "/project"

func TestMain(m *testing.M) {
	// buildah re-executes the test binary to run build steps.
	if buildah.InitReexec() {
		return
	}
	if os.Getenv("BUILDAH_ISOLATION") == "" {
		os.Setenv("BUILDAH_ISOLATION", "chroot")
	}
	os.Exit(m.Run())
}

// requireBuildah skips tests that build images when that isn't possible.
func requireBuildah(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds an image; skipped with -short")
	}
	if os.Geteuid() != 0 {
		t.Skip("builds an image, which needs root")
	}
}

// project returns an in-memory filesystem holding files, keyed by their path within
// projectDir.
func project(t *testing.T, files map[string]string) afero.Fs {
	t.Helper()
	fsys := afero.NewMemMapFs()
	if err := fsys.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := afero.WriteFile(fsys, filepath.Join(projectDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fsys
}

// diskProject is like project, but keeps the files on disk, under root/project, for tests
// that need symbolic links or that write into the project. Links maps a link's path within
// projectDir to its target.
func diskProject(t *testing.T, files, links map[string]string) (fsys afero.Fs, root string) {
	t.Helper()
	root = t.TempDir()
	fsys = afero.NewBasePathFs(afero.NewOsFs(), root)
	for name, content := range files {
		path := filepath.Join(projectDir, name)
		if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(fsys, path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		path := filepath.Join(root, projectDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	return fsys, root
}

// unnamedImages returns the IDs of the images in the local container store that have no
// name, as the images of a BUILD file's intermediate steps do.
func unnamedImages(t *testing.T) map[string]bool {
	t.Helper()
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.GetStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(false)
	images, err := store.Images()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, img := range images {
		if len(img.Names) == 0 {
			ids[img.ID] = true
		}
	}
	return ids
}

// assertNoNewImages fails the test if the local container store has unnamed images that
// aren't in before, such as an intermediate step's image left behind.
func assertNoNewImages(t *testing.T, before map[string]bool) {
	t.Helper()
	for id := range unnamedImages(t) {
		if !before[id] {
			t.Errorf("image %s left in the container store", id)
		}
	}
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

// vamosWithOutput is like vamos, but runs with ctx, as main does to handle Ctrl-C, and
// also copies stdout to watch, if it isn't nil.
func vamosWithOutput(t *testing.T, fsys afero.Fs, ctx context.Context, watch io.Writer, args ...string) result {
	t.Helper()
	cmd := NewRootCommand(fsys)
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
	if !strings.Contains(r.stdout, "Image built!") {
		t.Errorf("stdout = %q, want it to report the image built", r.stdout)
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
	if strings.Contains(r.stdout, "Image built!") {
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

// imageLink returns the target of the symbolic link at path in img's filesystem.
func imageLink(t *testing.T, img v1.Image, path string) string {
	t.Helper()
	var hdr *tar.Header
	walkImage(t, img, func(h *tar.Header, r io.Reader) bool {
		if imagePath(h) == path {
			hdr = h
		}
		return hdr != nil
	})
	if hdr == nil {
		t.Fatalf("%s not found in image", path)
	}
	if hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("%s is not a symbolic link in the image", path)
	}
	return hdr.Linkname
}

// imagePaths returns the paths of the files in img's filesystem under dir.
func imagePaths(t *testing.T, img v1.Image, dir string) []string {
	t.Helper()
	var paths []string
	walkImage(t, img, func(hdr *tar.Header, r io.Reader) bool {
		if path := imagePath(hdr); strings.HasPrefix(path, dir+"/") {
			paths = append(paths, path)
		}
		return false
	})
	return paths
}

// walkImage calls visit with each entry in img's flattened filesystem, until it returns true.
// It reports whether visit did so.
func walkImage(t *testing.T, img v1.Image, visit func(*tar.Header, io.Reader) bool) bool {
	t.Helper()
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false
		} else if err != nil {
			t.Fatal(err)
		}
		if visit(hdr, tr) {
			return true
		}
	}
}

// imagePath returns the absolute path within the image of a layer entry.
func imagePath(hdr *tar.Header) string {
	return "/" + strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/"), "/")
}

// fakeDocker serves the parts of the Docker Engine API that loading an image uses,
// over plain HTTP on a "unix" socket or a "tcp" port, and points DOCKER_HOST at it.
// It answers loads with reply, a stream of JSON messages, or reports success if reply
// is empty. It returns the tarballs loaded.
func fakeDocker(t *testing.T, network, reply string) func() [][]byte {
	t.Helper()
	var listener net.Listener
	var err error
	switch network {
	case "unix":
		// Unix socket paths are limited to about 100 bytes, so avoid t.TempDir's long names.
		dir, err := os.MkdirTemp("", "vamos-docker-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "docker.sock")
		listener, err = net.Listen("unix", sock)
		t.Setenv("DOCKER_HOST", "unix://"+sock)
	case "tcp":
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			t.Setenv("DOCKER_HOST", "tcp://"+listener.Addr().String())
		}
	default:
		t.Fatalf("fakeDocker: unknown network %q", network)
	}
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var loaded [][]byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			io.WriteString(w, "OK")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			loaded = append(loaded, b)
			mu.Unlock()
			body := reply
			if body == "" {
				body = `{"stream":"Loaded image\n"}`
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		default:
			t.Errorf("fake Docker daemon got unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return loaded
	}
}
