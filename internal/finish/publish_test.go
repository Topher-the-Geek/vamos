package finish

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	gcrname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// TestPublish checks that the client publishes each image to its repository under every tag, reporting each
// destination; that an upload that fails writes no tag; and that a tag that fails is reported with those written.
func TestPublish(t *testing.T) {
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	// The registry refuses every manifest written to the repository refused, and the tag v1 of untaggable.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") &&
			(strings.HasPrefix(r.URL.Path, "/v2/refused/") || r.URL.Path == "/v2/untaggable/app/manifests/v1") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errors": [{"code": "DENIED", "message": "writes refused"}]}`))
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	ref, id := pushRandomImage(t, host+"/vamos-cache", "linux/amd64")
	img := Image{Registry: ref}
	publication := func(repos ...string) []Publication {
		var images []Publication
		for _, repo := range repos {
			named, err := reference.ParseNormalizedNamed(host + "/" + repo)
			testenv.Must(t, err)
			images = append(images, Publication{Image: img, Repository: named})
		}
		return images
	}
	digestOf := func(image string) string {
		t.Helper()
		r, err := gcrname.ParseReference(image, gcrname.Insecure)
		testenv.Must(t, err)
		desc, err := remote.Head(r)
		if err != nil {
			return ""
		}
		return desc.Digest.String()
	}

	destinations, err := Publish(t.Context(), publication("team/app", "mirror/app"), []string{"v1", "latest"}, sys)
	testenv.Must(t, err)
	want := []api.Destination{
		{Repository: host + "/team/app", Tag: "v1", Image: id, Written: true},
		{Repository: host + "/team/app", Tag: "latest", Image: id, Written: true},
		{Repository: host + "/mirror/app", Tag: "v1", Image: id, Written: true},
		{Repository: host + "/mirror/app", Tag: "latest", Image: id, Written: true},
	}
	if !reflect.DeepEqual(destinations, want) {
		t.Errorf("destinations = %+v, want %+v", destinations, want)
	}
	first := digestOf(host + "/team/app:v1")
	for _, image := range []string{"team/app:latest", "mirror/app:v1", "mirror/app:latest"} {
		if got := digestOf(host + "/" + image); got != first {
			t.Errorf("%s names %q, want the manifest team/app:v1 names, %s", image, got, first)
		}
	}

	// An upload that fails writes no tag, even to the repositories uploaded to first.
	_, err = Publish(t.Context(), publication("first/app", "refused/app"), []string{"v1"}, sys)
	if err == nil || !strings.Contains(err.Error(), "uploading to "+host+"/refused/app") {
		t.Errorf("a refused upload = %v, want an error naming the repository", err)
	}
	if got := digestOf(host + "/first/app:v1"); got != "" {
		t.Errorf("after a failed upload, first/app:v1 names %s, want nothing", got)
	}

	// A tag that fails is reported with those written, and every tag is attempted.
	destinations, err = Publish(t.Context(), publication("untaggable/app", "tagged/app"), []string{"v1", "latest"}, sys)
	if err == nil || !strings.Contains(err.Error(), host+"/untaggable/app:v1") || !strings.Contains(err.Error(),
		"written: "+host+"/untaggable/app:latest, "+host+"/tagged/app:v1, "+host+"/tagged/app:latest") {
		t.Errorf("a refused tag = %v, want it named, and the tags written named too", err)
	}
	if len(destinations) != 4 || destinations[0].Written || !destinations[1].Written || !destinations[3].Written {
		t.Errorf("destinations = %+v, want untaggable/app:v1 alone unwritten", destinations)
	}
}
