package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	gclayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/opencontainers/go-digest"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/types"
)

// sourceImage writes a random image to an OCI layout and returns a reference to it.
func sourceImage(t *testing.T) types.ImageReference {
	t.Helper()
	img, err := random.Image(256, 1)
	testenv.Must(t, err)
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
	dir := t.TempDir()
	_, err = gclayout.Write(dir, index)
	testenv.Must(t, err)
	ref, err := layout.NewReference(dir, "")
	testenv.Must(t, err)
	return ref
}

// pushTo pushes src to the tag image at a registry over plain HTTP, and returns the manifest written, or the error.
func pushTo(t *testing.T, image string, src types.ImageReference) ([]byte, error) {
	t.Helper()
	named, err := reference.ParseNormalizedNamed(image)
	testenv.Must(t, err)
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	policyContext, err := Policy(sys)
	testenv.Must(t, err)
	defer policyContext.Destroy()
	return Push(t.Context(), policyContext, named.(reference.NamedTagged), src, sys,
		&imagecopy.Options{DestinationCtx: sys})
}

// quickRetries makes pushes retry twice without waiting, whatever the host's containers.conf says.
func quickRetries(t *testing.T) {
	t.Helper()
	was := Retries
	Retries = func() (*retry.Options, error) { return &retry.Options{MaxRetry: 2, Delay: time.Millisecond}, nil }
	t.Cleanup(func() { Retries = was })
}

// TestPushRetries checks that a push survives a registry that fails transiently, and reads the tag back.
func TestPushRetries(t *testing.T) {
	quickRetries(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var failures atomic.Int32
	failures.Store(2) // the first two manifest uploads fail
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") && failures.Add(-1) >= 0 {
			http.Error(w, "try again", http.StatusBadGateway)
			return
		}
		if isManifestRead(r) {
			reads.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	written, err := pushTo(t, strings.TrimPrefix(server.URL, "http://")+"/app:v1", sourceImage(t))
	testenv.Must(t, err)
	if len(written) == 0 {
		t.Error("Push returned no manifest")
	}
	if reads.Load() == 0 {
		t.Error("the tag wasn't read back after the push")
	}
}

// TestPushReadBackMismatch checks that a push fails when the tag names another manifest afterwards.
func TestPushReadBackMismatch(t *testing.T) {
	quickRetries(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	other, err := random.Image(128, 1)
	testenv.Must(t, err)
	otherManifest, err := other.RawManifest()
	testenv.Must(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isManifestRead(r) {
			// The tag has moved, as if another push won.
			w.Header().Set("Content-Type", mediaTypeOf(t, other))
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(otherManifest).String())
			if r.Method == http.MethodGet {
				w.Write(otherManifest)
			}
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, err = pushTo(t, strings.TrimPrefix(server.URL, "http://")+"/app:v1", sourceImage(t))
	if err == nil || !strings.Contains(err.Error(), "names manifest") ||
		!strings.Contains(err.Error(), "not the one pushed") {
		t.Errorf("Push = %v, want a mismatch after reading the tag back", err)
	}
}

// TestPushReadBackRetries checks that reading the tag back survives a registry that fails transiently.
func TestPushReadBackRetries(t *testing.T) {
	quickRetries(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var pushed atomic.Bool
	var failures atomic.Int32
	failures.Store(2) // the first two reads after the push fail
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			defer pushed.Store(true)
		} else if pushed.Load() && isManifestRead(r) && failures.Add(-1) >= 0 {
			// The connection drops, as a transient network failure does.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, err := pushTo(t, strings.TrimPrefix(server.URL, "http://")+"/app:v1", sourceImage(t))
	testenv.Must(t, err)
	if left := failures.Load(); left >= 0 {
		t.Errorf("%d of the failing reads were never made; the tag wasn't read back as the test means", left+1)
	}
}

// TestPushReadBackCancelled checks that a push whose context ends while it reads the tag back stops,
// rather than waiting for a registry that doesn't answer, or retrying.
func TestPushReadBackCancelled(t *testing.T) {
	quickRetries(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pushed atomic.Bool
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			defer pushed.Store(true)
		} else if pushed.Load() && isManifestRead(r) {
			reads.Add(1)
			cancel()
			<-r.Context().Done() // never answers
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(server.URL, "http://") + "/app:v1")
	testenv.Must(t, err)
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	policyContext, err := Policy(sys)
	testenv.Must(t, err)
	defer policyContext.Destroy()
	_, err = Push(ctx, policyContext, named.(reference.NamedTagged), sourceImage(t), sys,
		&imagecopy.Options{DestinationCtx: sys})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Push cancelled while reading back = %v, want %v", err, context.Canceled)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the tag was read back %d times, want once, without retrying once cancelled", n)
	}
}

// TestPushReadBackIgnoresMirrors checks that a push reads the tag back from the registry it wrote to,
// not from a mirror that registries.conf configures for pulling from it, which holds an older image by that tag.
func TestPushReadBackIgnoresMirrors(t *testing.T) {
	quickRetries(t)
	newRegistry := func() string {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		return strings.TrimPrefix(server.URL, "http://")
	}
	primary, mirror := newRegistry(), newRegistry()
	_, err := pushTo(t, mirror+"/app:v1", sourceImage(t))
	testenv.Must(t, err)

	conf := filepath.Join(t.TempDir(), "registries.conf")
	testenv.Must(t, os.WriteFile(conf, []byte(fmt.Sprintf(
		"[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n",
		primary, mirror)), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)

	written, err := pushTo(t, primary+"/app:v1", sourceImage(t))
	testenv.Must(t, err)
	if len(written) == 0 {
		t.Error("Push returned no manifest")
	}
}

// isManifestRead reports whether r reads a manifest, as reading a tag back does.
func isManifestRead(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.URL.Path, "/manifests/")
}

// mediaTypeOf returns img's manifest media type.
func mediaTypeOf(t *testing.T, img v1.Image) string {
	t.Helper()
	mt, err := img.MediaType()
	testenv.Must(t, err)
	return string(mt)
}
