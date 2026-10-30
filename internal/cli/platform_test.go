package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/run"

	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// native is the platform the tests run on, as vamos names it.
var native = platforms.Format(platforms.Normalize(platforms.DefaultSpec()))

// foreign returns a platform that isn't the host's, whose programs run here only under emulation.
func foreign() string {
	if runtime.GOARCH == "amd64" {
		return "linux/arm64"
	}
	return "linux/amd64"
}

// uname returns what `uname -m` prints on platform.
func uname(platform string) string {
	switch platform {
	case "linux/amd64":
		return "x86_64"
	case "linux/arm64":
		return "aarch64"
	}
	panic("no machine name for " + platform)
}

// requireEmulation skips the test if r failed because the platform it built for needs an emulator that isn't
// registered, and fails it if r failed otherwise.
func requireEmulation(t *testing.T, r result) {
	t.Helper()
	if r.err != nil && strings.Contains(r.err.Error(), "under emulation") {
		t.Skipf("no emulator for %s: %v", foreign(), r.err)
	}
	r.succeeds(t)
}

// TestPlatformMistakes checks the errors for platforms that can't be built for.
func TestPlatformMistakes(t *testing.T) {
	tests := []struct {
		name  string
		build string
		args  []string
		want  []string
	}{
		{
			name: "platform without an architecture",
			args: []string{"--platform", "linux"},
			want: []string{`--platform linux: platform "linux": want os/arch or os/arch/variant`},
		},
		{
			name: "platform that doesn't parse",
			args: []string{"--platform", "linux/amd64/v3/x"},
			want: []string{`--platform linux/amd64/v3/x: platform "linux/amd64/v3/x": want os/arch`},
		},
		{
			name:  "the platform as a parameter",
			build: `layer(name="app", params={"exec_platform": "linux/amd64"}, base="scratch")`,
			want: []string{
				"/project/BUILD:1:6:",
				`layer: params: "exec_platform" is built in, as ctx.exec_platform, and can't be declared`,
			},
		},
		{
			// Functions see the platform as a string, so comparing it with one works; fail() shows what they saw.
			name: "platform compared with a string",
			build: `layer(name="app", base="scratch", command=lambda ctx: fail(` +
				`ctx.platform == "linux/arm/v7", ctx.os, ctx.arch, ctx.variant, ` +
				`ctx.exec_platform == "` + native + `"))`,
			args: []string{"--platform", "linux/arm"},
			want: []string{"True linux arm v7 True"},
		},
		{
			name:  "a part of the platform as a parameter",
			build: `layer(name="app", params={"arch": "amd64"}, base="scratch")`,
			want: []string{
				"/project/BUILD:1:6:",
				`layer: params: "arch" is built in, as ctx.arch, and can't be declared`,
			},
		},
		{
			name: "execution platform without an architecture",
			args: []string{"--exec-platform", "linux"},
			want: []string{`--exec-platform linux: platform "linux": want os/arch or os/arch/variant`},
		},
		{
			name: "execution platform the executor doesn't offer",
			args: []string{"--exec-platform", "linux/s390x"},
			want: []string{`--exec-platform linux/s390x: the executor doesn't run commands on linux/s390x; ` +
				`it offers "` + native + `"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			build := tt.build
			if build == "" {
				build = `layer(name="app", base="scratch")`
			}
			fsys := project(t, map[string]string{"BUILD": build})
			vamos(t, fsys, append([]string{"build", "app", "-C", projectDir}, tt.args...)...).
				failsWith(t, false, tt.want...)
		})
	}
}

// TestExecPlatformChoice checks which execution platform the client configures a graph for, among those the
// executor offers: the one named, or else the one the executor says clients use, or else this machine's, or else the
// only one; several, none this machine's, must be chosen between. An older coordinator, which says none, is answered
// as before.
func TestExecPlatformChoice(t *testing.T) {
	other := imgspecv1.Platform{OS: "linux", Architecture: "s390x"}
	another := imgspecv1.Platform{OS: "linux", Architecture: "ppc64le"}
	host := platforms.Normalize(platforms.DefaultSpec())
	tests := []struct {
		name      string
		offered   []imgspecv1.Platform
		preferred *imgspecv1.Platform // the executor's
		flag      string
		want      string // the platform chosen, or "" for an error
		err       string
	}{
		{name: "this machine's among several", offered: []imgspecv1.Platform{other, host}, want: native},
		{
			name:      "the executor's, over this machine's",
			offered:   []imgspecv1.Platform{host, other},
			preferred: &other,
			want:      "linux/s390x",
		},
		{
			name:      "named, over the executor's",
			offered:   []imgspecv1.Platform{host, other},
			preferred: &other,
			flag:      native,
			want:      native,
		},
		{
			name:      "the executor's, not offered",
			offered:   []imgspecv1.Platform{other, host},
			preferred: &another,
			want:      native,
		},
		{name: "the only one", offered: []imgspecv1.Platform{other}, want: "linux/s390x"},
		{name: "named", offered: []imgspecv1.Platform{other, another}, flag: "linux/ppc64le", want: "linux/ppc64le"},
		{
			name:    "several, none this machine's",
			offered: []imgspecv1.Platform{other, another},
			err: "the executor doesn't run commands on this machine's platform, " + native +
				`; choose among "linux/s390x", "linux/ppc64le" with --exec-platform`,
		},
		{name: "none", err: "the executor offers no platform to run commands on"},
		{name: "none, named", flag: "linux/s390x", err: "the executor offers no platform to run commands on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := &invocation{opts: options{execPlatform: tt.flag}}
			got, err := inv.execPlatform(api.Capabilities{Platforms: tt.offered, ExecPlatform: tt.preferred})
			switch {
			case tt.err != "" && (err == nil || err.Error() != tt.err):
				t.Errorf("execPlatform = %v, want %q", err, tt.err)
			case tt.err == "" && (err != nil || platforms.Format(got) != tt.want):
				t.Errorf("execPlatform = %s, %v; want %s", platforms.Format(got), err, tt.want)
			}
		})
	}
	// An older coordinator's answer has no execution platform for clients: this machine's is chosen, as before.
	var older api.Capabilities
	must(t, json.Unmarshal(fmt.Appendf(nil, `{"platforms":[{"os":"linux","architecture":"s390x"},`+
		`{"os":%q,"architecture":%q}],"graph_versions":[%d]}`, host.OS, host.Architecture, graph.Version), &older))
	if got, err := (&invocation{}).execPlatform(older); err != nil || platforms.Format(got) != native {
		t.Errorf("execPlatform of an older coordinator = %s, %v; want this machine's, %s", platforms.Format(got),
			err, native)
	}

	// Naming this machine's platform builds as without the flag.
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": `layer(name="app", base="scratch")`})
	vamos(t, fsys, "build", "app", "--exec-platform", native, "-C", projectDir).ran(t)
}

// platformBuild has a layer that runs a command for its platform.
// nonce makes the target unique to a test run.
func platformBuild(nonce string) string {
	return pullBusybox + `
layer(
    name="machine",
    base="busybox",
    steps=[run(["sh", "-c", "echo ` + nonce + ` $(uname -m) > /machine.txt"])],
)
`
}

// TestPlatforms builds a layer for the host's platform and another: its image and its runs are for its platform,
// and its result's provenance says whether its commands ran under emulation.
func TestPlatforms(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": platformBuild(nonce)})
	dir := t.TempDir()

	for i, platform := range []string{native, foreign()} {
		before := emulatedResults(t)
		out := filepath.Join(dir, fmt.Sprintf("machine-%d.tar", i))
		r := vamos(t, fsys, "save", "machine", out, "--platform", platform, "-C", projectDir)
		requireEmulation(t, r)
		img := readImage(t, out)
		assertPlatform(t, img, platform)
		if got, want := imageFile(t, img, "/machine.txt"), nonce+" "+uname(platform)+"\n"; got != want {
			t.Errorf("%s: /machine.txt = %q, want %q", platform, got, want)
		}
		after := emulatedResults(t)
		emulated := platform != native
		if after[emulated] <= before[emulated] || after[!emulated] != before[!emulated] {
			t.Errorf("%s: results with emulated provenance went from %v to %v (false, true); want one more with %v",
				platform, before, after, emulated)
		}
	}
}

// emulatedResults counts the action results in the suite's build cache by whether their provenance says their
// commands ran under emulation, reading the cache's own files, as a test may.
func emulatedResults(t *testing.T) map[bool]int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(run.CacheDir(stores(t).Options), "ac"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[bool]int{} // nothing has been built in this store yet
	}
	must(t, err)
	counts := map[bool]int{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(run.CacheDir(stores(t).Options), "ac", e.Name()))
		must(t, err)
		var result cache.ActionResult
		must(t, json.Unmarshal(data, &result))
		counts[result.Provenance != nil && result.Provenance.Emulated]++
	}
	return counts
}

// TestPlatformFailures checks the errors for layers that can't be built for their platform.
func TestPlatformFailures(t *testing.T) {
	requireBuildah(t)
	t.Run("base for another platform", func(t *testing.T) {
		fsys := project(t, map[string]string{
			"BUILD": pullBusybox +
				fmt.Sprintf(`layer(name="app", base=dep("busybox", platform="%s"))`, foreign()),
		})
		out := t.TempDir()
		vamos(t, fsys, "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).failsWith(t, false,
			fmt.Sprintf(`layer "app": its base busybox is for %s, not %s; a layer's base must be for its platform`,
				foreign(), native))
		assertNoFiles(t, out)
	})
	t.Run("run under emulation fails", func(t *testing.T) {
		fsys := project(t, map[string]string{
			"BUILD": pullBusybox + fmt.Sprintf(`layer(name="app", base="busybox", steps=[run(["false"])]) # %s`,
				newNonce()),
		})
		vamos(t, fsys, "build", "app", "--platform", foreign(), "-C", projectDir).failsWith(t, false,
			fmt.Sprintf(`layer "app": run(["false"]) failed: exit status 1; the image is for %s, which runs here `+
				`under emulation: "exec format error" above means no emulator for it is registered`, foreign()))
	})
}

// assertPlatform checks that img is for platform, including its variant, both normalized as containerd does.
func assertPlatform(t *testing.T, img v1.Image, platform string) {
	t.Helper()
	config, err := img.ConfigFile()
	must(t, err)
	want, err := platforms.Parse(platform)
	must(t, err)
	got := platforms.Normalize(imgspecv1.Platform{
		OS:           config.OS,
		Architecture: config.Architecture,
		Variant:      config.Variant,
	})
	if platforms.Format(got) != platforms.Format(want) {
		t.Errorf("image is for %s, want %s", platforms.Format(got), platforms.Format(want))
	}
}

// pushIndex pushes to ref an index of a random image for each of platforms, returning them by platform.
func pushIndex(t *testing.T, ref string, platformNames ...string) map[string]v1.Image {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	must(t, err)
	images := map[string]v1.Image{}
	var index v1.ImageIndex = empty.Index
	for _, platform := range platformNames {
		os, arch, _ := strings.Cut(platform, "/")
		img := randomImage(t, platform)
		images[platform] = img
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}},
		})
	}
	must(t, remote.WriteIndex(r, index))
	return images
}

// randomImage returns an image of random files for platform.
func randomImage(t *testing.T, platform string) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	must(t, err)
	config, err := img.ConfigFile()
	must(t, err)
	config.OS, config.Architecture, _ = strings.Cut(platform, "/")
	img, err = mutate.ConfigFile(img, config)
	must(t, err)
	return img
}

// assertBase checks that img, a layer's image, is built on base: that it starts with base's layers.
// what says which image base is, for the message.
func assertBase(t *testing.T, img, base v1.Image, what string) {
	t.Helper()
	got, want := layerDigests(t, img), layerDigests(t, base)
	if got = got[:min(len(got), len(want))]; !slices.Equal(got, want) {
		t.Errorf("image starts with layers %v, want those of %s, %v", got, what, want)
	}
}

// layerDigests returns the digests of img's layers.
func layerDigests(t *testing.T, img v1.Image) []v1.Hash {
	t.Helper()
	layers, err := img.Layers()
	must(t, err)
	var digests []v1.Hash
	for _, l := range layers {
		d, err := l.DiffID()
		must(t, err)
		digests = append(digests, d)
	}
	return digests
}

// TestPullTag checks that a pull's tag is resolved once and recorded:
// each platform's image comes from the index it named then, even after the tag moves,
// and for a platform not built for before, until --rebuild resolves it again.
// Pruning keeps the record and the pulled images, so builds after it don't reach the registry.
// It has stores of its own, since it prunes them.
func TestPullTag(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var requests atomic.Int64 // that pulls make, of the image "base"
	server, regHost := insecureRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/base/") {
			requests.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	first := pushIndex(t, regHost+"/base:v1", native, foreign())

	fsys := project(t, map[string]string{
		"BUILD": fmt.Sprintf(`pull(name="base", image="%s/base", tag="v1")`+"\n"+
			`layer(name="app", base="base")`, regHost),
	})
	dir := t.TempDir()
	write := func(name, platform string) v1.Image {
		t.Helper()
		out := filepath.Join(dir, strings.ReplaceAll(name, "/", "-"))
		vamos(t, fsys, "save", "app", out, "--platform", platform, "--tls-verify=false", "-C", projectDir).
			succeeds(t)
		return readImage(t, out)
	}
	img := write("host-1.tar", native)
	assertPlatform(t, img, native)
	assertBase(t, img, first[native], "the index's image for "+native)

	// The tag moves, but builds keep using what it named, without asking the registry.
	second := pushIndex(t, regHost+"/base:v1", native, foreign())
	before := requests.Load()
	img = write("host-2.tar", native)
	if n := requests.Load() - before; n != 0 {
		t.Errorf("a build after the images were pulled made %d requests to the registry, want none", n)
	}
	assertBase(t, img, first[native], "the image the tag named before it moved")

	// A platform not built for before gets its image from the index recorded, not the one the tag names now.
	img = write("other-2.tar", foreign())
	assertPlatform(t, img, foreign())
	assertBase(t, img, first[foreign()], "the recorded index's image for "+foreign()+", not built for before")

	r := vamos(t, fsys, "build", "base", "--rebuild", "--tls-verify=false", "-C", projectDir).ran(t)
	if want := "pull \"base\": pulling again\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	for _, platform := range []string{native, foreign()} {
		img := write(platform+"-3.tar", platform)
		assertBase(t, img, second[platform], "the image the tag names now for "+platform)
	}

	// With the registry gone, the layers built again after pruning use the images pulled before, and the pulled
	// image itself is delivered from the store, not from the registry its result also locates it at.
	vamos(t, fsys, "prune").ran(t)
	server.Close()
	for _, platform := range []string{native, foreign()} {
		img := write(platform+"-4.tar", platform)
		assertBase(t, img, second[platform], "the image the tag named before pruning, for "+platform)
	}
	out := filepath.Join(dir, "base-offline.tar")
	vamos(t, fsys, "save", "base", out, "--platform", native, "-C", projectDir).succeeds(t)
	assertBase(t, readImage(t, out), second[native], "the pulled image, saved with the registry gone")
}

// TestPullTagMistakes checks the errors for a tag that names no image for the platform.
func TestPullTagMistakes(t *testing.T) {
	requireBuildah(t)
	_, regHost := insecureRegistry(t, nil)
	pushIndex(t, regHost+"/index:v1", "linux/s390x", "linux/ppc64le")
	ref, err := name.ParseReference(regHost+"/single:v1", name.Insecure)
	must(t, err)
	must(t, remote.Write(ref, randomImage(t, "linux/s390x")))

	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "index without the platform",
			image: "index",
			want: fmt.Sprintf(`its index has no image for %s; it has "linux/ppc64le", "linux/s390x"`,
				native),
		},
		{
			name:  "image for another platform",
			image: "single",
			want: fmt.Sprintf("is for linux/s390x, not %s, and its tag names that image alone, "+
				"not an index of images for several platforms", native),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"BUILD": fmt.Sprintf(`pull(name="base", image="%s/%s", tag="v1")`+"\n"+
					`layer(name="app", base="base")`, regHost, tt.image),
			})
			vamos(t, fsys, "build", "app", "--tls-verify=false", "-C", projectDir).
				failsWith(t, false, fmt.Sprintf(`pull "base": image "%s/%s:v1"`, regHost, tt.image), tt.want)
		})
	}
}
