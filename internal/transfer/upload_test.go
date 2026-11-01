package transfer

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	gclayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/opencontainers/go-digest"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/types"
)

// sourceIndex writes a random index of two images to an OCI layout and returns a reference to it.
func sourceIndex(t *testing.T) types.ImageReference {
	t.Helper()
	index, err := random.Index(256, 1, 2)
	testenv.Must(t, err)
	dir := t.TempDir()
	_, err = gclayout.Write(dir, mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: index}))
	testenv.Must(t, err)
	ref, err := layout.NewReference(dir, "")
	testenv.Must(t, err)
	return ref
}

// sources are the kinds of thing a publication uploads: an image, and an index.
var sources = []struct {
	name   string
	source func(*testing.T) types.ImageReference
}{
	{
		name:   "image",
		source: sourceImage,
	},
	{
		name:   "index",
		source: sourceIndex,
	},
}

// plainHTTP is the registry settings of a test's registry, which it serves over plain HTTP.
var plainHTTP = &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}

// uploadTo uploads src to the repository repo at a registry over plain HTTP, copying every image of an index,
// and returns the manifest written.
func uploadTo(t *testing.T, repo string, src types.ImageReference) []byte {
	t.Helper()
	named, err := reference.ParseNormalizedNamed(repo)
	testenv.Must(t, err)
	policyContext, err := Policy(plainHTTP)
	testenv.Must(t, err)
	defer policyContext.Destroy()
	written, err := Upload(t.Context(), policyContext, named, src, &imagecopy.Options{
		DestinationCtx:     plainHTTP,
		ImageListSelection: imagecopy.CopyAllImages,
	})
	testenv.Must(t, err)
	return written
}

// tagAs writes the manifest uploaded to the tag image, at a registry over plain HTTP.
func tagAs(t *testing.T, image string, uploaded []byte) error {
	t.Helper()
	named, err := reference.ParseNormalizedNamed(image)
	testenv.Must(t, err)
	return Tag(t.Context(), named.(reference.NamedTagged), uploaded, plainHTTP)
}

// tagsOf lists the tags of the repository repo at the registry whose address is host.
func tagsOf(t *testing.T, host, repo string) []string {
	t.Helper()
	resp, err := http.Get("http://" + host + "/v2/" + repo + "/tags/list")
	testenv.Must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	testenv.Must(t, json.NewDecoder(resp.Body).Decode(&list))
	return list.Tags
}

// manifestDigest returns the digest of the manifest that ref, a tag or digest in the repository repo, names
// at the registry whose address is host, or "" if it names none.
func manifestDigest(t *testing.T, host, repo, ref string) string {
	t.Helper()
	resp, err := http.Head("http://" + host + "/v2/" + repo + "/manifests/" + ref)
	testenv.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// newTestRegistry starts an in-process registry over plain HTTP, whose requests pass through observe first,
// if given, and returns its address.
func newTestRegistry(t *testing.T, observe func(*http.Request)) string {
	t.Helper()
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// TestUploadLeavesNoTag checks that an upload of an image or an index writes its manifest by digest,
// which can be read by it, and leaves the repository with no tag.
func TestUploadLeavesNoTag(t *testing.T) {
	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			quickRetries(t)
			host := newTestRegistry(t, nil)
			written := uploadTo(t, host+"/team/app", tc.source(t))
			if tags := tagsOf(t, host, "team/app"); len(tags) != 0 {
				t.Errorf("after an upload, the repository has tags %v, want none", tags)
			}
			want := digest.FromBytes(written).String()
			if got := manifestDigest(t, host, "team/app", want); got != want {
				t.Errorf("reading the uploaded manifest by digest gave %q, want %s", got, want)
			}
		})
	}
}

// TestTagNamesTheUpload checks that a tag names the manifest uploaded, for an image and for an index,
// that tagging adds the tag alone, and that it sends no blob.
func TestTagNamesTheUpload(t *testing.T) {
	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			quickRetries(t)
			var tagging atomic.Bool
			var blobWrites atomic.Int32
			host := newTestRegistry(t, func(r *http.Request) {
				if tagging.Load() && strings.Contains(r.URL.Path, "/blobs/") &&
					r.Method != http.MethodGet && r.Method != http.MethodHead {
					blobWrites.Add(1)
				}
			})
			uploaded := uploadTo(t, host+"/team/app", tc.source(t))
			want := digest.FromBytes(uploaded).String()
			tagging.Store(true)
			for _, tag := range []string{"v1", "latest"} {
				testenv.Must(t, tagAs(t, host+"/team/app:"+tag, uploaded))
				if got := manifestDigest(t, host, "team/app", tag); got != want {
					t.Errorf("tag %s names manifest %q, want the one uploaded, %s", tag, got, want)
				}
			}
			tags := tagsOf(t, host, "team/app")
			slices.Sort(tags)
			if want := []string{"latest", "v1"}; !slices.Equal(tags, want) {
				t.Errorf("tags = %v, want %v", tags, want)
			}
			if n := blobWrites.Load(); n != 0 {
				t.Errorf("tagging wrote blobs %d times, want none", n)
			}
		})
	}
}

// TestTagReadBackMismatch checks that tagging fails, naming the tag, when it names another manifest afterwards.
func TestTagReadBackMismatch(t *testing.T) {
	quickRetries(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	other, err := random.Image(128, 1)
	testenv.Must(t, err)
	otherManifest, err := other.RawManifest()
	testenv.Must(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isManifestRead(r) && strings.HasSuffix(r.URL.Path, "/manifests/v1") {
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
	host := strings.TrimPrefix(server.URL, "http://")
	uploaded := uploadTo(t, host+"/team/app", sourceImage(t))
	err = tagAs(t, host+"/team/app:v1", uploaded)
	if err == nil || !strings.Contains(err.Error(), host+"/team/app:v1 names manifest") ||
		!strings.Contains(err.Error(), "not the one uploaded, "+digest.FromBytes(uploaded).String()) {
		t.Errorf("Tag = %v, want a mismatch naming the tag after reading it back", err)
	}
}

// TestTagIgnoresMirrors checks that tagging an index needs nothing from a mirror that registries.conf configures
// for pulling from the registry, even one that holds none of what was uploaded.
func TestTagIgnoresMirrors(t *testing.T) {
	quickRetries(t)
	var mirrorRequests atomic.Int32
	primary := newTestRegistry(t, nil)
	mirror := newTestRegistry(t, func(*http.Request) { mirrorRequests.Add(1) })
	uploaded := uploadTo(t, primary+"/team/app", sourceIndex(t))

	conf := filepath.Join(t.TempDir(), "registries.conf")
	testenv.Must(t, os.WriteFile(conf, []byte(fmt.Sprintf(
		"[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n",
		primary, mirror)), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)

	testenv.Must(t, tagAs(t, primary+"/team/app:v1", uploaded))
	if n := mirrorRequests.Load(); n != 0 {
		t.Errorf("tagging sent the mirror %d requests, want none", n)
	}
}

// TestTagRetries checks that tagging survives a registry that fails transiently as the tag is written.
func TestTagRetries(t *testing.T) {
	quickRetries(t)
	var failures atomic.Int32
	failures.Store(2) // the first two writes of the tag fail
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/v1") && failures.Add(-1) >= 0 {
			http.Error(w, "try again", http.StatusBadGateway)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	uploaded := uploadTo(t, host+"/team/app", sourceImage(t))
	testenv.Must(t, tagAs(t, host+"/team/app:v1", uploaded))
	if left := failures.Load(); left >= 0 {
		t.Errorf("%d of the failing writes were never made; the tag wasn't retried as the test means", left+1)
	}
	if got, want := manifestDigest(t, host, "team/app", "v1"), digest.FromBytes(uploaded).String(); got != want {
		t.Errorf("tag v1 names manifest %q, want the one uploaded, %s", got, want)
	}
}
