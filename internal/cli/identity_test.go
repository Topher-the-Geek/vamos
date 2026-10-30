package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/run"
	containersconfig "go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/manifest"
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

// TestMovedCacheName checks that an image is used through its name in the cache repository only if what has the name
// is that image: with the name moved to another image, as `podman tag` can do,
// the layer is built again rather than taken from the other image, and its image gets its name back.
func TestMovedCacheName(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	a := project(t, map[string]string{"BUILD": scratchLayer("app", nonce+"a"), "app.sh": "echo a"})
	b := project(t, map[string]string{"BUILD": scratchLayer("app", nonce+"b"), "app.sh": "echo b"})
	idA := builtImage(t, func() { vamos(t, a, "build", "app", "-C", projectDir).ran(t) })
	idB := builtImage(t, func() { vamos(t, b, "build", "app", "-C", projectDir).ran(t) })
	withStore(t, func(store storage.Store) {
		img, err := store.Image(run.CacheRepository + ":" + idB)
		must(t, err)
		must(t, store.AddNames(img.ID, []string{run.CacheRepository + ":" + idA}))
	})

	r := vamos(t, a, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "building, since the cached result can't be used: " +
		"its image " + idA[:12] + " in the container store: the image named for it isn't that image: " +
		"it is image " + idB[:12]})
	withStore(t, func(store storage.Store) {
		img, err := store.Image(run.CacheRepository + ":" + idA)
		must(t, err)
		if img.ID != idA {
			t.Errorf("the name for %s names store image %s, want the image the layer made before", idA, img.ID)
		}
	})
}

// TestUnverifiedLayers checks that an image whose layers in the container store aren't those its config lists,
// as a partial pull can leave with storage.conf's insecure_allow_unpredictable_image_contents,
// isn't used through the config's digest: its config would identify files it doesn't have.
// The test makes such an image, with files of its own under another's config, so it has stores of its own.
func TestUnverifiedLayers(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	fsys := project(t, map[string]string{"BUILD": scratchLayer("app", newNonce()), "app.sh": "echo hi"})
	id := builtImage(t, func() { vamos(t, fsys, "build", "app", "-C", projectDir).ran(t) })
	withStore(t, func(store storage.Store) {
		img, err := store.Image(run.CacheRepository + ":" + id)
		must(t, err)
		var files bytes.Buffer
		w := tar.NewWriter(&files)
		must(t, w.WriteHeader(&tar.Header{Name: "app.sh", Mode: 0o755, Size: 9}))
		_, err = w.Write([]byte("echo bye\n"))
		must(t, err)
		must(t, w.Close())
		layer, _, err := store.PutLayer("", "", nil, "", false, nil, &files)
		must(t, err)
		unverified, err := store.CreateImage("", nil, layer.ID, "",
			&storage.ImageOptions{CreationDate: img.Created, Digest: img.Digest})
		must(t, err)
		copyImageData(t, store, img, unverified.ID)
		must(t, store.AddNames(unverified.ID, []string{run.CacheRepository + ":" + id}))
	})

	r := vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "building, since the cached result can't be used: " +
		"its image " + id[:12] + " in the container store: the image named for it isn't that image: " +
		"the image's layers in the container store aren't those its config lists, as when storage.conf's " +
		"insecure_allow_unpredictable_image_contents lets a partial pull skip checking them"})
}

// TestImageStoreIDs checks that images whose container store IDs aren't their image IDs, the digests of their configs,
// are found by their image IDs: a cached layer is used, built on, and sent, alone and in an index.
// The images are recreated under new store IDs, so the test has stores of its own.
func TestImageStoreIDs(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": indexBuild(nonce) + `
glob(name="more", include=["more.txt"])
layer(name="next", base="app", steps=[add("more", "/more")])
`,
		"app.sh":   "echo " + nonce,
		"more.txt": "more\n",
	})
	id := builtImage(t, func() { vamos(t, fsys, "build", "app", "-C", projectDir).ran(t) })
	vamos(t, fsys, "build", "server", "-C", projectDir).ran(t)
	rekeyImages(t)
	withStore(t, func(store storage.Store) {
		img, err := store.Image(run.CacheRepository + ":" + id)
		must(t, err)
		if img.ID == id {
			t.Fatalf("image %s still has its image ID as its store ID", id)
		}
	})

	r := vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached"})
	r = vamos(t, fsys, "build", "server", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{
		`layer "app"`: "cached",
		fmt.Sprintf(`layer "app" (platform=%s)`, foreign()): "cached",
	})
	r = vamos(t, fsys, "build", "next", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached", `layer "next"`: "building"})

	_, host := insecureRegistry(t, nil)
	r = vamos(t, fsys, "push", "app", host+"/app:v1", "--tls-verify=false", "-C", projectDir).succeeds(t)
	if got := builtID(t, r); got != id {
		t.Errorf("pushed image %s, want %s", got, id)
	}
	vamos(t, fsys, "push", "server", host+"/server:v1", "--tls-verify=false", "-C", projectDir).succeeds(t)
}

// builtImage returns the ID of the one image that build adds to the cache repository,
// which, as committed, is both its image ID and its store ID.
func builtImage(t *testing.T, build func()) string {
	t.Helper()
	before := cachedImages(t)
	build()
	var added []string
	for id := range cachedImages(t) {
		if !before[id] {
			added = append(added, id)
		}
	}
	if len(added) != 1 {
		t.Fatalf("the build added images %q to the cache repository, want one", added)
	}
	return added[0]
}

// rekeyImages recreates each image named in the cache repository under a random store ID,
// with its layers, data, digests and names, as a store can hold an image whose store ID isn't its config's digest.
func rekeyImages(t *testing.T) {
	t.Helper()
	withStore(t, func(store storage.Store) {
		images, err := store.Images()
		must(t, err)
		for _, img := range images {
			if !slices.ContainsFunc(img.Names, func(name string) bool {
				return strings.HasPrefix(name, run.CacheRepository+":")
			}) {
				continue
			}
			copied, err := store.CreateImage("", nil, img.TopLayer, img.Metadata,
				&storage.ImageOptions{CreationDate: img.Created, Digest: img.Digest})
			must(t, err)
			copyImageData(t, store, &img, copied.ID)
			// The copy keeps the layers in use as the original is deleted.
			_, err = store.DeleteImage(img.ID, true)
			must(t, err)
			must(t, store.AddNames(copied.ID, img.Names))
		}
	})
}

// copyImageData copies img's data, its manifests and config, to the image with store ID to in store.
func copyImageData(t *testing.T, store storage.Store, img *storage.Image, to string) {
	t.Helper()
	for _, key := range img.BigDataNames {
		data, err := store.ImageBigData(img.ID, key)
		must(t, err)
		must(t, store.SetImageBigData(to, key, data, manifest.Digest))
	}
}

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
