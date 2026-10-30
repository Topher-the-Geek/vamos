package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/run"
	containersconfig "go.podman.io/common/pkg/config"
	"go.podman.io/storage"
)

// TestImageIdentityRoundTrip checks that an image's ID is portable: pushed to a registry and pulled into a fresh
// container store, it is reported with the ID the push reported, and the store names it by that ID
// in the cache repository, as it names the layers it builds, which is how run finds it.
func TestImageIdentityRoundTrip(t *testing.T) {
	requireBuildah(t)
	_, host := insecureRegistry(t, nil)
	r := vamos(t, project(t, helloBuild), "push", "app", host+"/team/app:v1", "--tls-verify=false", "-C", projectDir).
		succeeds(t)
	id := builtID(t, r)

	usePrivateStores(t)
	fsys := project(t, map[string]string{
		"BUILD": `pull(name="base", image="` + host + `/team/app", tag="v1")`,
	})
	out := filepath.Join(t.TempDir(), "base.tar")
	r = vamos(t, fsys, "save", "base", out, "--tls-verify=false", "-C", projectDir).succeeds(t)
	if pulled := builtID(t, r); pulled != id {
		t.Errorf("the pulled image's ID is %s, want the pushed image's, %s", pulled, id)
	}
	withStore(t, func(store storage.Store) {
		if _, err := store.Image(run.CacheRepository + ":" + id); err != nil {
			t.Errorf("the fresh store has no image named by the ID the push reported, %s: %v", id, err)
		}
	})
}

// TestResultProvenance checks that each result, a layer's and a task's, records what made it:
// the executor's platform, even for a layer built for another, its isolation, capabilities and version;
// and that using a cached result leaves its record as it was.
// The build has stores of its own, so that its records are the only ones.
func TestResultProvenance(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD":     cachedBuild(nonce) + scratchLayer("other", nonce),
		"input.txt": "hello\n",
		"app.sh":    "echo " + nonce,
	})
	vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	vamos(t, fsys, "build", "other", "--platform", foreign(), "-C", projectDir).ran(t)
	records := recordsNaming(t, "epoch")
	if len(records) != 4 {
		t.Fatalf("%d results recorded, want the three layers' and the task's", len(records))
	}

	conf, err := containersconfig.Default()
	must(t, err)
	capabilities, err := conf.Capabilities("root", nil, nil)
	must(t, err)
	want := cache.Provenance{Platform: native, Isolation: "chroot", Capabilities: capabilities, Version: version(t)}
	written := map[string][]byte{}
	foreignResults := 0
	for _, record := range records {
		data, err := os.ReadFile(record)
		must(t, err)
		written[record] = data
		var result cache.ActionResult
		must(t, json.Unmarshal(data, &result))
		if result.Provenance == nil || !reflect.DeepEqual(*result.Provenance, want) {
			t.Errorf("provenance = %+v, want %+v", result.Provenance, want)
		}
		if result.Platform == foreign() {
			foreignResults++
		}
	}
	if foreignResults != 1 {
		t.Errorf("%d results are for %s, want the layer built for it", foreignResults, foreign())
	}

	r := vamos(t, fsys, "build", "other", "--platform", foreign(), "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "other"`: "cached"})
	vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	for record, data := range written {
		if now, err := os.ReadFile(record); err != nil || !bytes.Equal(now, data) {
			t.Errorf("result %s changed when used from the cache: %v", filepath.Base(record), err)
		}
	}
}

// version returns the version of vamos that a result made by this test binary records:
// its main module's, unless that is unknown, as for a development build.
func version(t *testing.T) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "(devel)" {
		return ""
	}
	return info.Main.Version
}

// scratchLayer is a layer, name, on scratch that adds app.sh and runs nothing, so that it builds for any platform.
// nonce makes it unique to a test run.
func scratchLayer(name, nonce string) string {
	return fmt.Sprintf(`
glob(name="%[1]s_code", include=["app.sh"])
layer(
    name="%[1]s",
    base="scratch",
    steps=[add("%[1]s_code", "/")],
    command=["/app.sh", "%[2]s"],
)
`, name, nonce)
}
