package run

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
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

	m := cache.NewMemory()
	// The tag now names the second index, as another job's --rebuild recorded.
	testenv.Must(t, m.SetTag(t.Context(), image, &cache.TagRecord{Digest: second}))
	pl := &puller{records: m, systemContext: &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}}
	named, err := reference.ParseNamed(image)
	testenv.Must(t, err)
	tagged := named.(reference.NamedTagged)
	platform := imgspecv1.Platform{OS: "linux", Architecture: "amd64"}

	instance, record, err := pl.instance(t.Context(), tagged, platform, first, false)
	testenv.Must(t, err)
	if instance != firstAMD64 || record.Digest != first || digest.FromBytes(record.Manifest) != first {
		t.Errorf("pinned resolve = %s with record of %s, want %s from the first index, %s", instance, record.Digest,
			firstAMD64, first)
	}

	// A registry that gives the second index for the first's digest is refused, whatever the tag record says.
	swap.from, swap.to = first.String(), second.String()
	_, _, err = pl.instance(t.Context(), tagged, platform, first, false)
	if want := "the registry gave manifest " + second.String() + " for " + first.String(); err == nil ||
		err.Error() != want {
		t.Errorf("pinned resolve from a lying registry = %v, want %q", err, want)
	}
}
