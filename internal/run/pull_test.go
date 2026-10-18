package run

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// TestPullerPinnedIndex checks that a resolve pinned to an index uses that index after the tag record has moved on,
// fetching it from the registry by its digest, and that a registry answering with another manifest is refused.
func TestPullerPinnedIndex(t *testing.T) {
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	// swap, when set, makes the registry answer a request for one manifest by digest with another.
	var swap struct{ from, to string }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if swap.from != "" && strings.HasSuffix(r.URL.Path, "/manifests/"+swap.from) {
			r.URL.Path = strings.TrimSuffix(r.URL.Path, swap.from) + swap.to
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	image := strings.TrimPrefix(server.URL, "http://") + "/team/base:v1"
	push := func() (index, amd64 digest.Digest) {
		t.Helper()
		img, err := random.Image(64, 1)
		testenv.Must(t, err)
		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		})
		ref, err := name.ParseReference(image, name.Insecure)
		testenv.Must(t, err)
		testenv.Must(t, remote.WriteIndex(ref, idx))
		d, err := idx.Digest()
		testenv.Must(t, err)
		id, err := img.Digest()
		testenv.Must(t, err)
		return digest.Digest(d.String()), digest.Digest(id.String())
	}
	first, firstAMD64 := push()
	second, _ := push()

	// The tag now names the second index, as another job's --rebuild recorded.
	recorded := &cache.TagRecord{Digest: second}
	pl := &puller{systemContext: &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}}
	named, err := reference.ParseNamed(image)
	testenv.Must(t, err)
	tagged := named.(reference.NamedTagged)
	platform := imgspecv1.Platform{OS: "linux", Architecture: "amd64"}

	instance, record, err := pl.instance(t.Context(), tagged, platform, first, recorded)
	testenv.Must(t, err)
	if instance != firstAMD64 || record.Digest != first || digest.FromBytes(record.Manifest) != first {
		t.Errorf("pinned resolve = %s with record of %s, want %s from the first index, %s", instance, record.Digest,
			firstAMD64, first)
	}
	if recorded.Digest != second || recorded.Instances != nil {
		t.Errorf("the record given = %+v after resolving; want it unchanged, the scheduler's own", recorded)
	}

	// A registry that gives the second index for the first's digest is refused, whatever the tag record says.
	swap.from, swap.to = first.String(), second.String()
	_, _, err = pl.instance(t.Context(), tagged, platform, first, recorded)
	if want := "the registry gave manifest " + second.String() + " for " + first.String(); err == nil ||
		err.Error() != want {
		t.Errorf("pinned resolve from a lying registry = %v, want %q", err, want)
	}
}

// TestMaterializeFromRegistry checks that a runner whose store lacks a base image fetches it from a registry location,
// checks that what arrives is the image asked for, by its ID, and names it in the store, so that the next use finds
// it there without the registry; that an image of another ID at the location is refused;
// that a location that can't be parsed gives way to the next; that a location naming an index gives the image
// for the base's platform, though not the host's; and that a result naming that image is unusable for another
// platform, as a tag record swapped between platforms would be.
func TestMaterializeFromRegistry(t *testing.T) {
	testenv.RequireBuildah(t)
	ctx := t.Context()
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	testenv.InsecureRegistry(t, host)
	img, err := random.Image(64, 1)
	testenv.Must(t, err)
	ref, err := name.ParseReference(host+"/team/base:v1", name.Insecure)
	testenv.Must(t, err)
	testenv.Must(t, remote.Write(ref, img))
	manifestDigest, err := img.Digest()
	testenv.Must(t, err)
	configDigest, err := img.ConfigName()
	testenv.Must(t, err)
	id := configDigest.Hex
	location := cache.Location{Kind: cache.LocationRegistry, Reference: host + "/team/base@" + manifestDigest.String()}

	l, err := NewLocal(testenv.PrivateStores(t).Options)
	testenv.Must(t, err)
	defer l.Close()
	testenv.Must(t, l.open())
	r := l.runner
	unset := imgspecv1.Platform{} // the host's image, as a single image is fetched whatever its platform
	if _, err := r.materialize(ctx, id, unset, nil); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("materializing an image the store lacks, with no location = %v, want %v", err, storage.ErrImageUnknown)
	}
	wrong := strings.Repeat("0", 64)
	_, err = r.materialize(ctx, wrong, unset, []cache.Location{location})
	if err == nil || !strings.Contains(err.Error(), "it is image "+id[:12]+", not "+wrong[:12]) {
		t.Errorf("materializing another ID from the location = %v, want the image's ID refused", err)
	}
	malformed := cache.Location{Kind: cache.LocationRegistry, Reference: "Not A Reference"}
	storeID, err := r.materialize(ctx, id, unset, []cache.Location{malformed, location})
	testenv.Must(t, err)
	if located, _, err := locateImage(ctx, r.store, id); err != nil || located != storeID {
		t.Errorf("after materializing, the image is located as %s, %v; want %s by its name", located, err, storeID)
	}
	before := requests.Load()
	if again, err := r.materialize(ctx, id, unset, []cache.Location{location}); err != nil || again != storeID {
		t.Errorf("materializing again = %s, %v; want %s from the store", again, err, storeID)
	}
	if n := requests.Load() - before; n != 0 {
		t.Errorf("materializing an image the store holds made %d requests to the registry, want none", n)
	}

	// A tag naming an index of two platforms' images: the one for the base's platform must arrive.
	foreign := imgspecv1.Platform{OS: "linux", Architecture: "arm64"}
	if platforms.NewMatcher(foreign).Match(HostPlatform) {
		foreign.Architecture = "amd64"
	}
	var idx v1.ImageIndex = empty.Index
	for _, p := range []imgspecv1.Platform{HostPlatform, foreign} {
		img, err := random.Image(64, 1)
		testenv.Must(t, err)
		config, err := img.ConfigFile()
		testenv.Must(t, err)
		config.OS, config.Architecture = p.OS, p.Architecture
		img, err = mutate.ConfigFile(img, config)
		testenv.Must(t, err)
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: p.OS, Architecture: p.Architecture}},
		})
	}
	tag, err := name.ParseReference(host+"/team/multi:v1", name.Insecure)
	testenv.Must(t, err)
	testenv.Must(t, remote.WriteIndex(tag, idx))
	manifest, err := idx.IndexManifest()
	testenv.Must(t, err)
	foreignImage, err := idx.Image(manifest.Manifests[1].Digest)
	testenv.Must(t, err)
	foreignConfig, err := foreignImage.ConfigName()
	testenv.Must(t, err)
	tagged := cache.Location{Kind: cache.LocationRegistry, Reference: tag.String()}
	if _, err := r.materialize(ctx, foreignConfig.Hex, foreign, []cache.Location{tagged}); err != nil {
		t.Fatalf("materializing the %s image of a tag naming an index = %v, want it chosen for its platform",
			platforms.Format(foreign), err)
	}
	result := &cache.ActionResult{Image: digest.Digest(foreignConfig.String()), Platform: platforms.Format(foreign)}
	for want, usable := range map[string]bool{platforms.Format(foreign): true, platforms.Format(HostPlatform): false} {
		a := &sched.Action{Version: sched.ActionVersion, Kind: sched.KindResolve, Image: tag.String(), Platform: want}
		problem, err := r.Check(ctx, a, result)
		testenv.Must(t, err)
		if (problem == nil) != usable {
			t.Errorf("checking the %s image for %s = %v, want usable %t", platforms.Format(foreign), want, problem,
				usable)
		}
	}
}
