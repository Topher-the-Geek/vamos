package cli

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// indexBuild has an index of a layer on scratch,
// which runs nothing, for the host's platform and another, listed in that order.
// The index passes its own parameter on to the layer.
// nonce makes the targets unique to a test run.
func indexBuild(nonce string) string {
	return fmt.Sprintf(`
glob(name="code", include=["app.sh"])
layer(
    name="app",
    params={"greeting": "hi"},
    base="scratch",
    steps=[add("code", "/")],
    command=lambda ctx: ["/app.sh", "%s", ctx.greeting],
)
index(
    name="server",
    params={"greeting": "hi"},
    layer=dep("app", greeting=arg("greeting")),
    platforms=["%s", "%s"],
)
`, nonce, native, foreign())
}

// TestIndex loads an index, which sends the manifest for the platform of the Docker daemon.
func TestIndex(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": indexBuild(nonce), "app.sh": "echo " + nonce})

	loaded := fakeDocker(t, "unix", "")
	vamos(t, fsys, "load", "server", "server:dev", "-C", projectDir).succeeds(t)
	images := loaded()
	if len(images) != 1 {
		t.Fatalf("loaded %d images, want 1", len(images))
	}
	img, err := tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(images[0])), nil
	}, nil)
	must(t, err)
	// The fake daemon runs on amd64.
	assertPlatform(t, img, "linux/amd64")
}

// TestIndexChoosesManifest checks that load, which sends one image of an index, configures only the manifest it
// sends: another, which fails as it is configured, doesn't fail it.
func TestIndexChoosesManifest(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": fmt.Sprintf(`
layer(name="app", base="scratch", command=["app", "%s"])
layer(name="broken", base=lambda ctx: fail("configured broken for " + ctx.platform))
index(name="server", layer={"linux/s390x": "broken", "linux/amd64": "app", "linux/arm64": "app"})
`, nonce)})

	loaded := fakeDocker(t, "unix", "")
	r := vamos(t, fsys, "load", "server", "server:dev", "-C", projectDir).succeeds(t)
	if strings.Contains(r.stdout, "broken") {
		t.Errorf("stdout = %q, want only the daemon's manifest built", r.stdout)
	}
	if images := loaded(); len(images) != 1 {
		t.Errorf("loaded %d images, want 1", len(images))
	}
}

// TestPlatformVariants checks that load chooses the manifest for the daemon's variant,
// where a manifest for another variant of the same architecture, listed first, doesn't match.
func TestPlatformVariants(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": fmt.Sprintf(`
layer(name="app", base="scratch", command=lambda ctx: ["app", "%s", ctx.variant])
index(name="server", layer="app", platforms=["linux/arm/v6", "linux/arm/v7"])
`, nonce)})

	loaded := fakeDockerOn(t, "unix", "", "armv7l")
	vamos(t, fsys, "load", "server", "server:dev", "-C", projectDir).succeeds(t)
	images := loaded()
	if len(images) != 1 {
		t.Fatalf("loaded %d images, want 1", len(images))
	}
	img, err := tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(images[0])), nil
	}, nil)
	must(t, err)
	assertPlatform(t, img, "linux/arm/v7")
	if cmd := imageCmd(t, img); !slices.Equal(cmd, []string{"app", nonce, "v7"}) {
		t.Errorf("loaded command = %q, want the v7 manifest's", cmd)
	}
}
