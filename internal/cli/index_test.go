package cli

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// TestIndexMistakes checks the errors for indexes that can't be built, or sent as they are named.
func TestIndexMistakes(t *testing.T) {
	layers := `
glob(name="code", include=["app.sh"])
layer(name="app", params={"mode": "release"}, base="scratch", steps=[add("code", "/")])
task(name="build", base="app", source="/", include=["app.sh"])
`
	tests := []struct {
		name  string
		build string
		args  []string
		want  []string
	}{
		{
			name:  "no platforms",
			build: `index(name="server", layer="app", platforms=[])`,
			want:  []string{"/project/BUILD:5:6:", "index: platforms must not be empty"},
		},
		{
			name:  "platforms missing",
			build: `index(name="server", layer="app")`,
			want: []string{
				"/project/BUILD:5:6:",
				"index: platforms is missing: the platforms to build layer for",
				"or give layer as a dict from each platform to its target",
			},
		},
		{
			name:  "platform that doesn't parse",
			build: `index(name="server", layer="app", platforms=["linux/amd64", "x86"])`,
			want:  []string{"/project/BUILD:5:6:", `index: platforms[1]: platform "x86": want os/arch`},
		},
		{
			name:  "a platform twice",
			build: `index(name="server", layer="app", platforms=["linux/arm64", "linux/amd64", "linux/arm64/v8"])`,
			want: []string{
				"/project/BUILD:5:6:",
				"index: platforms[0] and platforms[2] are both linux/arm64; an index has one image for each platform",
			},
		},
		{
			name:  "layer not a label",
			build: `index(name="server", layer=1, platforms=["linux/amd64"])`,
			want:  []string{"/project/BUILD:5:6:", "index: layer is int, want a label or dep()"},
		},
		{
			name:  "layer a task",
			build: `index(name="server", layer="build", platforms=["linux/amd64"])`,
			want:  []string{"/project/BUILD:5:6:", `index: layer "build" is a task(), not a pull() or layer()`},
		},
		{
			name: "layer an index",
			build: `index(name="inner", layer="app", platforms=["linux/amd64"])
index(name="server", layer="inner", platforms=["linux/amd64"])`,
			want: []string{"/project/BUILD:6:6:", `index: layer "inner" is an index(), not a pull() or layer()`},
		},
		{
			name: "index as a base",
			build: `index(name="inner", layer="app", platforms=["linux/amd64"])
layer(name="server", base="inner")`,
			want: []string{"/project/BUILD:6:6:", `layer: base "inner" is an index(), not a pull() or layer()`},
		},
		{
			name:  "layer's dep gives the platform",
			build: `index(name="server", layer=dep("app", platform="linux/amd64"), platforms=["linux/amd64"])`,
			want: []string{
				"/project/BUILD:5:6:",
				"index: layer: an index's dep() can't give platform, since platforms[0] says what each manifest is for",
			},
		},
		{
			name:  "dict and platforms",
			build: `index(name="server", layer={"linux/amd64": "app"}, platforms=["linux/amd64"])`,
			want: []string{
				"/project/BUILD:5:6:",
				"index: give platforms, or a dict for layer, whose keys are the platforms, not both",
			},
		},
		{
			name:  "empty dict",
			build: `index(name="server", layer={})`,
			want:  []string{"/project/BUILD:5:6:", "index: layer must not be empty"},
		},
		{
			name:  "dict's dep gives the platform",
			build: `index(name="server", layer={"linux/amd64": dep("app", platform=EXEC_PLATFORM)})`,
			want: []string{
				"/project/BUILD:5:6:",
				`index: layer["linux/amd64"]: an index's dep() can't give platform, since layer["linux/amd64"]'s key ` +
					"says what each manifest is for",
			},
		},
		{
			name:  "dict's keys a platform twice",
			build: `index(name="server", layer={"linux/arm": "app", "linux/arm/v7": "app"})`,
			want: []string{
				"/project/BUILD:5:6:",
				`index: layer["linux/arm"]'s key and layer["linux/arm/v7"]'s key are both linux/arm/v7`,
			},
		},
		{
			name:  "arg the index doesn't declare",
			build: `index(name="server", layer=dep("app", mode=arg("mode")), platforms=["linux/amd64"])`,
			want: []string{
				"/project/BUILD:5:47:",
				`arg: "server" has no parameter "mode" to pass on; it declares none`,
			},
		},
		{
			name:  "platform given",
			build: `index(name="server", layer="app", platforms=["linux/amd64"])`,
			args:  []string{"--platform", foreign()},
			want: []string{`"server" is an index(), whose manifests are each built for their own platform; ` +
				"name one of them to build it for the platform given"},
		},
		{
			name:  "argument given that the index doesn't declare",
			build: `index(name="server", layer="app", platforms=["linux/amd64"])`,
			args:  []string{"--set", "mode=debug"},
			want:  []string{`--set mode=debug: "server" has no parameter "mode"; it declares none`},
		},
		{
			name:  "no manifest for the destination",
			build: `index(name="server", layer="app", platforms=["linux/s390x"])`,
			want: []string{
				`index "server" has no manifest for ` + native + `, the host's platform; it has "linux/s390x"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": layers + tt.build, "app.sh": "echo hi"})
			out := t.TempDir()
			args := append([]string{"save", "server", filepath.Join(out, "app.tar"), "-C", projectDir}, tt.args...)
			vamos(t, fsys, args...).failsWith(t, false, tt.want...)
			assertNoFiles(t, out)
		})
	}
}

// indexBuild has an index of a layer on scratch, which runs nothing, for the host's platform and another,
// listed in that order. The index passes its own parameter on to the layer. nonce makes the targets unique to a
// test run.
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

// TestIndex builds an index, and sends it: push sends every manifest, in order, with its platform, while save
// and load, which take one image, send the manifest for the platform of the machine that will run it.
func TestIndex(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": indexBuild(nonce), "app.sh": "echo " + nonce})

	r := vamos(t, fsys, "build", "server", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{
		`layer "app"`: "building",
		fmt.Sprintf(`layer "app" (platform=%s)`, foreign()): "building",
	})

	t.Run("push", func(t *testing.T) {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		image := strings.TrimPrefix(server.URL, "http://") + "/server:v1"
		r := vamos(t, fsys, "push", "server", image, "--set", "greeting=hola", "--tls-verify=false", "-C", projectDir).
			succeeds(t)
		assertTargets(t, r, map[string]string{
			`layer "app" (greeting="hola")`:                                      "building",
			fmt.Sprintf(`layer "app" (platform=%s, greeting="hola")`, foreign()): "building",
		})

		ref, err := name.ParseReference(image, name.Insecure)
		must(t, err)
		index, err := remote.Index(ref)
		must(t, err)
		digest, err := index.Digest()
		must(t, err)
		if !strings.HasSuffix(r.stdout, "Index built! "+image+" "+digest.String()+"\n") {
			t.Errorf("stdout = %q, want it to end with the index's digest, %s", r.stdout, digest)
		}
		manifest, err := index.IndexManifest()
		must(t, err)
		var got []string
		for _, m := range manifest.Manifests {
			got = append(got, m.Platform.OS+"/"+m.Platform.Architecture)
			img, err := index.Image(m.Digest)
			must(t, err)
			assertPlatform(t, img, m.Platform.OS+"/"+m.Platform.Architecture)
			if content := imageFile(t, img, "/app.sh"); content != "echo "+nonce {
				t.Errorf("%s: /app.sh = %q, want the file", m.Platform, content)
			}
			if cmd := imageCmd(t, img); !slices.Equal(cmd, []string{"/app.sh", nonce, "hola"}) {
				t.Errorf("%s: command = %q, want the index's argument passed on", m.Platform, cmd)
			}
		}
		if want := []string{native, foreign()}; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("manifests are for %q, want %q, in order", got, want)
		}
	})

	t.Run("push from the client", func(t *testing.T) {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		image := strings.TrimPrefix(server.URL, "http://") + "/server:v1"
		r := vamos(t, fsys, "push", "server", image, "--set", "greeting=hola", "--push-from=client",
			"--tls-verify=false", "-C", projectDir).succeeds(t)

		ref, err := name.ParseReference(image, name.Insecure)
		must(t, err)
		index, err := remote.Index(ref)
		must(t, err)
		digest, err := index.Digest()
		must(t, err)
		if !strings.HasSuffix(r.stdout, "Index built! "+image+" "+digest.String()+"\n") {
			t.Errorf("stdout = %q, want it to end with the index's digest, %s", r.stdout, digest)
		}
		manifest, err := index.IndexManifest()
		must(t, err)
		var got []string
		for _, m := range manifest.Manifests {
			got = append(got, m.Platform.OS+"/"+m.Platform.Architecture)
		}
		if want := []string{native, foreign()}; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("manifests are for %q, want %q, in order", got, want)
		}
	})

	t.Run("save", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "server.tar")
		vamos(t, fsys, "save", "server", out, "-C", projectDir).succeeds(t)
		assertPlatform(t, readImage(t, out), native)
	})

	t.Run("load", func(t *testing.T) {
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
	})
}

// TestIndexByPlatform builds an index given a different target for each platform, as a dict.
func TestIndexByPlatform(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": fmt.Sprintf(`
layer(name="host", base="scratch", command=["host", "%s"])
layer(name="other", base="scratch", command=["other", "%s"])
index(name="server", layer={"%s": "other", "%s": dep("host")})
`, nonce, nonce, foreign(), native)})

	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	image := strings.TrimPrefix(server.URL, "http://") + "/server:v1"
	vamos(t, fsys, "push", "server", image, "--tls-verify=false", "-C", projectDir).succeeds(t)

	ref, err := name.ParseReference(image, name.Insecure)
	must(t, err)
	index, err := remote.Index(ref)
	must(t, err)
	manifest, err := index.IndexManifest()
	must(t, err)
	var got []string
	for _, m := range manifest.Manifests {
		platform := m.Platform.OS + "/" + m.Platform.Architecture
		img, err := index.Image(m.Digest)
		must(t, err)
		assertPlatform(t, img, platform)
		got = append(got, platform+" "+imageCmd(t, img)[0])
	}
	if want := []string{foreign() + " other", native + " host"}; !slices.Equal(got, want) {
		t.Errorf("manifests are %q, want %q, in order", got, want)
	}
}

// TestIndexChoosesManifest checks that save and load, which send one image of an index, configure only the
// manifest they send: another, which fails as it is configured, fails only a command that builds every manifest.
func TestIndexChoosesManifest(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": fmt.Sprintf(`
layer(name="app", base="scratch", command=["app", "%s"])
layer(name="broken", base=lambda ctx: fail("configured broken for " + ctx.platform))
index(name="server", layer={"linux/s390x": "broken", "linux/amd64": "app", "linux/arm64": "app"})
`, nonce)})

	vamos(t, fsys, "build", "server", "-C", projectDir).failsWith(t, false, "configured broken for linux/s390x")

	out := filepath.Join(t.TempDir(), "server.tar")
	r := vamos(t, fsys, "save", "server", out, "-C", projectDir).succeeds(t)
	assertPlatform(t, readImage(t, out), native)
	if strings.Contains(r.stdout, "broken") {
		t.Errorf("stdout = %q, want only the host's manifest built", r.stdout)
	}

	loaded := fakeDocker(t, "unix", "")
	r = vamos(t, fsys, "load", "server", "server:dev", "-C", projectDir).succeeds(t)
	if strings.Contains(r.stdout, "broken") {
		t.Errorf("stdout = %q, want only the daemon's manifest built", r.stdout)
	}
	if images := loaded(); len(images) != 1 {
		t.Errorf("loaded %d images, want 1", len(images))
	}
}

// TestPlatformVariants checks that a platform's variant reaches each place it is given: the image a layer
// commits, an index's descriptors, and the manifest chosen for a destination, where a manifest for another
// variant of the same architecture, listed first, doesn't match.
func TestPlatformVariants(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": fmt.Sprintf(`
layer(name="app", base="scratch", command=lambda ctx: ["app", "%s", ctx.variant])
index(name="server", layer="app", platforms=["linux/arm/v6", "linux/arm/v7"])
`, nonce)})

	out := filepath.Join(t.TempDir(), "app.tar")
	vamos(t, fsys, "save", "app", out, "--platform", "linux/arm/v7", "-C", projectDir).succeeds(t)
	img := readImage(t, out)
	assertPlatform(t, img, "linux/arm/v7")
	if cmd := imageCmd(t, img); !slices.Equal(cmd, []string{"app", nonce, "v7"}) {
		t.Errorf("command = %q, want ctx.variant v7", cmd)
	}

	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	image := strings.TrimPrefix(server.URL, "http://") + "/server:v1"
	vamos(t, fsys, "push", "server", image, "--tls-verify=false", "-C", projectDir).succeeds(t)
	ref, err := name.ParseReference(image, name.Insecure)
	must(t, err)
	index, err := remote.Index(ref)
	must(t, err)
	manifest, err := index.IndexManifest()
	must(t, err)
	var got []string
	for _, m := range manifest.Manifests {
		platform := m.Platform.OS + "/" + m.Platform.Architecture + "/" + m.Platform.Variant
		got = append(got, platform)
		img, err := index.Image(m.Digest)
		must(t, err)
		assertPlatform(t, img, platform)
	}
	if want := []string{"linux/arm/v6", "linux/arm/v7"}; !slices.Equal(got, want) {
		t.Errorf("manifests are for %q, want %q, in order", got, want)
	}

	loaded := fakeDockerOn(t, "unix", "", "armv7l")
	vamos(t, fsys, "load", "server", "server:dev", "-C", projectDir).succeeds(t)
	images := loaded()
	if len(images) != 1 {
		t.Fatalf("loaded %d images, want 1", len(images))
	}
	img, err = tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(images[0])), nil
	}, nil)
	must(t, err)
	assertPlatform(t, img, "linux/arm/v7")
	if cmd := imageCmd(t, img); !slices.Equal(cmd, []string{"app", nonce, "v7"}) {
		t.Errorf("loaded command = %q, want the v7 manifest's", cmd)
	}
}
