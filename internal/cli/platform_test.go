package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/run"

	"github.com/containerd/platforms"
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
}

// requireEmulation skips the test if r failed because the platform it built for needs an emulator that isn't
// registered, as buildah's "exec format error" says, and fails it if r failed otherwise.
func requireEmulation(t *testing.T, r result) {
	t.Helper()
	if r.err != nil && strings.Contains(r.stdout+r.stderr, "exec format error") {
		t.Skipf("no emulator for %s: %v", foreign(), r.err)
	}
	r.succeeds(t)
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

// TestPlatforms builds a layer for the host's platform and another, whose result's provenance says whether its
// commands ran under emulation, which it reads from the cache's own files. The platforms feature specifies what the
// image holds; this stays until the cache's provenance is specified as a user sees it.
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
