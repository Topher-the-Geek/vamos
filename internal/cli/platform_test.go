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

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/run"

	"github.com/containerd/platforms"
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
