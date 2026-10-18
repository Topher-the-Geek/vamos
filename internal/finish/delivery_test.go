package finish

import (
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/containerd/platforms"
	gcrname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	gcrtypes "github.com/google/go-containerregistry/pkg/v1/types"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

func TestMain(m *testing.M) {
	// buildah re-executes the test binary to run build steps.
	if buildah.InitReexec() {
		return
	}
	if os.Getenv("BUILDAH_ISOLATION") == "" {
		os.Setenv("BUILDAH_ISOLATION", "chroot")
	}
	code := m.Run()
	if err := testenv.RemoveSuiteStores(); err != nil {
		os.Stderr.WriteString("removing the suite's stores: " + err.Error() + "\n")
		code = 1
	}
	os.Exit(code)
}

// newExecutor returns an executor on private stores, which t's cleanup closes, and its content store.
func newExecutor(t *testing.T) (*run.Local, cache.ContentStore) {
	t.Helper()
	stores := testenv.PrivateStores(t)
	ex, err := run.NewLocal(stores.Options)
	testenv.Must(t, err)
	t.Cleanup(func() { ex.Close() })
	store, err := ex.ContentStore()
	testenv.Must(t, err)
	return ex, store
}

// sourceNode stores a tree in store that holds a.txt, of content, and returns a source node of it.
func sourceNode(t *testing.T, store cache.ContentStore, content string) graph.Node {
	t.Helper()
	ctx := t.Context()
	file, err := store.PutBytes(ctx, []byte(content))
	testenv.Must(t, err)
	tree, err := store.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	return graph.Node{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}}
}

// layerNode returns a layer on scratch, for platform, that adds the source node 0 at /data.
func layerNode(platform string) graph.Node {
	from := graph.Ref(0)
	return graph.Node{Label: "app", Desc: fmt.Sprintf(`layer "app" (%s)`, platform), Layer: &graph.Layer{
		Base: graph.Scratch, Platform: platform,
		Steps: []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}},
	}}
}

// buildExported has ex build g, from store, and export its root, returning the root's result and the export.
func buildExported(
	t *testing.T, ex api.Executor, store cache.ContentStore, g *graph.Graph,
) (api.Result, api.Exported) {
	t.Helper()
	testenv.Must(t, g.Validate())
	stored, err := store.PutBytes(t.Context(), graph.Encode(g))
	testenv.Must(t, err)
	outcome := finishJob(t, ex, api.Request{Graph: &api.GraphRequest{
		Graph: stored, Version: graph.Version, Platform: run.HostPlatform,
	}})
	res := outcome.Results[0]
	return res, *finishJob(t, ex, api.Request{Export: &api.ExportRequest{Result: res}}).Exported
}

// copyExported copies the image or index of exp from the executor's store, from, to a client's store in memory.
func copyExported(t *testing.T, from cache.ContentStore, exp api.Exported) *cache.Memory {
	t.Helper()
	clientStore := cache.NewMemory()
	blobs := append([]cache.Blob{exp.Manifest}, exp.Blobs...)
	copied, err := cache.Copy(t.Context(), from, clientStore, blobs)
	testenv.Must(t, err)
	if copied != len(blobs) {
		t.Errorf("copied %d blobs, want %d", copied, len(blobs))
	}
	return clientStore
}

// TestDeliverFromSeparateStore checks that the client's finish delivers an image from a content store of its own,
// never from the executor's directory: the executor on one directory exports an image's blobs,
// a client copies them into a store in memory, and Save writes a tarball from that store alone.
func TestDeliverFromSeparateStore(t *testing.T) {
	testenv.RequireBuildah(t)
	ex, executorStore := newExecutor(t)
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		sourceNode(t, executorStore, "delivered"), layerNode(platform),
	}}
	res, exported := buildExported(t, ex, executorStore, g)
	clientStore := copyExported(t, executorStore, exported)
	ex.Close() // the executor's stores take no part in delivery

	path := filepath.Join(t.TempDir(), "app.tar")
	id, err := Save(t.Context(), Exported(clientStore, exported), path, nil, &types.SystemContext{})
	testenv.Must(t, err)
	img, err := tarball.ImageFromPath(path, nil)
	testenv.Must(t, err)
	config, err := img.ConfigName()
	testenv.Must(t, err)
	if id != config.Hex || id != res.Image {
		t.Errorf("Save reported image %s; the tarball holds %s and the executor built %s", id, config.Hex, res.Image)
	}
}

// TestDeliverIndexFromSeparateStore checks that the client's finish pushes an index, and its images,
// from a content store of its own, with the executor's stores closed.
func TestDeliverIndexFromSeparateStore(t *testing.T) {
	testenv.RequireBuildah(t)
	ex, executorStore := newExecutor(t)
	host := platforms.Format(run.HostPlatform)
	other := "linux/arm64" // a layer that only adds files builds for any platform
	if host == other {
		other = "linux/amd64"
	}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: host, Roots: []graph.Ref{3}, Nodes: []graph.Node{
		sourceNode(t, executorStore, "indexed"), layerNode(host), layerNode(other),
		{Label: "all", Desc: `index "all"`, Index: &graph.Index{Manifests: []graph.Ref{1, 2}}},
	}}
	res, exported := buildExported(t, ex, executorStore, g)
	if len(res.Index) != 2 || exported.MediaType != imgspecv1.MediaTypeImageIndex {
		t.Fatalf("built %+v and exported %+v, want an index of two images", res, exported)
	}
	clientStore := copyExported(t, executorStore, exported)
	ex.Close()

	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(server.URL, "http://") + "/all:v1")
	testenv.Must(t, err)
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	pushed, err := Push(t.Context(), Exported(clientStore, exported), named.(reference.NamedTagged), sys)
	testenv.Must(t, err)
	if pushed != exported.Manifest.Digest.String() {
		t.Errorf("Push reported index %s, want the one exported, %s", pushed, exported.Manifest.Digest)
	}
	ref, err := gcrname.ParseReference(named.String(), gcrname.Insecure)
	testenv.Must(t, err)
	index, err := remote.Index(ref)
	testenv.Must(t, err)
	m, err := index.IndexManifest()
	testenv.Must(t, err)
	if len(m.Manifests) != 2 {
		t.Fatalf("the registry's index lists %d images, want 2", len(m.Manifests))
	}
	for i, d := range m.Manifests {
		img, err := index.Image(d.Digest)
		testenv.Must(t, err)
		config, err := img.ConfigName()
		testenv.Must(t, err)
		if config.Hex != res.Index[i].Image {
			t.Errorf("the index's image %d is %s, want %s", i, config.Hex, res.Index[i].Image)
		}
	}
}

// TestDeliverFilesFromSeparateStore checks that the client's finish exports a task's files from a content store
// of its own, with the executor's stores closed.
func TestDeliverFilesFromSeparateStore(t *testing.T) {
	testenv.RequireBuildah(t)
	testenv.RequireOwnership(t)
	ex, executorStore := newExecutor(t)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := graph.Ref(0)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platforms.Format(run.HostPlatform), Roots: []graph.Ref{1},
		Nodes: []graph.Node{
			sourceNode(t, executorStore, "exported"),
			{Label: "copy", Desc: `task "copy"`, Task: &graph.Task{
				Base:   graph.Scratch,
				Steps:  []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/out")`, From: &from, Dest: "/out"}},
				Output: &graph.Output{Source: "/out", Selection: sel},
			}},
		}}
	_, exported := buildExported(t, ex, executorStore, g)
	if exported.Files == nil {
		t.Fatalf("exported %+v, want a task's files", exported)
	}
	tree, err := cache.ReadTree(t.Context(), executorStore, *exported.Files)
	testenv.Must(t, err)
	clientStore := cache.NewMemory()
	_, err = cache.Copy(t.Context(), executorStore, clientStore, append([]cache.Blob{*exported.Files}, tree.Files()...))
	testenv.Must(t, err)
	ex.Close()

	dir := filepath.Join(t.TempDir(), "out")
	testenv.Must(t, Export(t.Context(), clientStore, exported, dir, false, func(err error) { t.Error(err) }))
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	testenv.Must(t, err)
	if string(data) != "exported" {
		t.Errorf("a.txt holds %q, want %q", data, "exported")
	}
}

// finishJob submits req to ex, watches it to its end and returns its outcome, which must be a success.
func finishJob(t *testing.T, ex api.Executor, req api.Request) api.Outcome {
	t.Helper()
	ctx := t.Context()
	id, err := ex.Submit(ctx, req)
	testenv.Must(t, err)
	testenv.Must(t, ex.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("Result = %+v, %v; want success", outcome, err)
	}
	return outcome
}

// TestDeliverFromRegistry checks that the client delivers an image from the registry location its result names,
// reading nothing from any content store, and assembles an index from its members' locations:
// what a coordinator's results, whose images workers pushed to the cache registry, give it.
func TestDeliverFromRegistry(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	push := func(platform string) (ref string, id string) {
		t.Helper()
		// An OCI image of one random layer, as vamos builds them, so that assembling the index converts nothing
		// and the image IDs hold.
		layer, err := random.Layer(256, gcrtypes.OCILayer)
		testenv.Must(t, err)
		img, err := mutate.AppendLayers(mutate.ConfigMediaType(mutate.MediaType(empty.Image,
			gcrtypes.OCIManifestSchema1), gcrtypes.OCIConfigJSON), layer)
		testenv.Must(t, err)
		config, err := img.ConfigFile()
		testenv.Must(t, err)
		config.OS, config.Architecture, _ = strings.Cut(platform, "/")
		img, err = mutate.ConfigFile(img, config)
		testenv.Must(t, err)
		configName, err := img.ConfigName()
		testenv.Must(t, err)
		ref = host + "/vamos-cache:" + configName.Hex
		r, err := gcrname.ParseReference(ref, gcrname.Insecure)
		testenv.Must(t, err)
		testenv.Must(t, remote.Write(r, img))
		return ref, configName.Hex
	}
	ref, id := push("linux/amd64")
	res := api.Result{Image: id, Locations: []cache.Location{
		{Kind: cache.LocationStore, Reference: "localhost/vamos-cache:" + id},
		{Kind: cache.LocationRegistry, Reference: ref},
	}}
	img, ok, err := FromLocations(res)
	if !ok || err != nil || img.Registry != ref {
		t.Fatalf("FromLocations = %+v, %v, %v; want the registry location", img, ok, err)
	}
	path := filepath.Join(t.TempDir(), "app.tar")
	saved, err := Save(t.Context(), img, path, nil, sys)
	testenv.Must(t, err)
	tar, err := tarball.ImageFromPath(path, nil)
	testenv.Must(t, err)
	config, err := tar.ConfigName()
	testenv.Must(t, err)
	if saved != id || config.Hex != id {
		t.Errorf("Save reported %s and the tarball holds %s, want %s from the registry", saved, config.Hex, id)
	}
	if _, ok, err := FromLocations(api.Result{Image: id, Locations: res.Locations[:1]}); ok || err != nil {
		t.Errorf("a result located in a store alone = %v, %v; want it not deliverable from a registry", ok, err)
	}
	broken := api.Result{Index: []api.Manifest{{Image: id, Platform: "linux-amd64", Locations: res.Locations[1:]}}}
	if _, _, err := FromLocations(broken); err == nil || !strings.Contains(err.Error(), "manifest 0") {
		t.Errorf("FromLocations of a member with platform %q = %v, want an error naming the manifest", "linux-amd64",
			err)
	}

	// An index is assembled from its members' locations, and pushed with every image.
	otherRef, otherID := push("linux/arm64")
	index := api.Result{Index: []api.Manifest{
		{Image: id, Platform: "linux/amd64", Locations: res.Locations[1:]},
		{Image: otherID, Platform: "linux/arm64", Locations: []cache.Location{{Kind: cache.LocationRegistry,
			Reference: otherRef}}},
	}}
	img, ok, err = FromLocations(index)
	if !ok || err != nil || len(img.Members) != 2 {
		t.Fatalf("FromLocations of an index = %+v, %v, %v; want two members", img, ok, err)
	}
	named, err := reference.ParseNormalizedNamed(host + "/all:v1")
	testenv.Must(t, err)
	pushed, err := Push(t.Context(), img, named.(reference.NamedTagged), sys)
	testenv.Must(t, err)
	r, err := gcrname.ParseReference(named.String(), gcrname.Insecure)
	testenv.Must(t, err)
	idx, err := remote.Index(r)
	testenv.Must(t, err)
	m, err := idx.IndexManifest()
	testenv.Must(t, err)
	d, err := idx.Digest()
	testenv.Must(t, err)
	if pushed != d.String() || len(m.Manifests) != 2 {
		t.Fatalf("Push reported %s; the registry's index %s lists %d images, want 2", pushed, d, len(m.Manifests))
	}
	for i, want := range []string{id, otherID} {
		member, err := idx.Image(m.Manifests[i].Digest)
		testenv.Must(t, err)
		config, err := member.ConfigName()
		testenv.Must(t, err)
		if config.Hex != want || m.Manifests[i].Platform.Architecture != index.Index[i].Platform[len("linux/"):] {
			t.Errorf("the index's image %d is %s for %v, want %s for %s", i, config.Hex, m.Manifests[i].Platform, want,
				index.Index[i].Platform)
		}
	}
}
