// Package run builds a graph, as an Executor: it pulls images, runs layers and tasks with buildah, reusing their
// cached results, and pushes or exports what it built.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/parse"
	containersconfig "go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// Executor runs build graphs and holds the images they make. Local runs them in this process; a remote
// executor could stand in for it, since a graph reaches it only through the build cache.
type Executor interface {
	// ExecPlatform returns the platform that run() steps run on without emulation.
	ExecPlatform(ctx context.Context) (imgspecv1.Platform, error)
	// Run runs the graph req names, reporting progress to ev, and returns a result for each of its roots.
	Run(ctx context.Context, req Request, ev Events) ([]Result, error)
	// Push copies a result's image or index from the executor to a registry, using the executor's credentials,
	// and returns its image ID there, or an index's digest.
	Push(ctx context.Context, res Result, dest reference.Named) (string, error)
	// Export copies a result's image or index to dest, a destination on the client, which sys describes, and
	// returns its image ID there, or an index's digest.
	Export(ctx context.Context, res Result, dest types.ImageReference, sys *types.SystemContext) (string, error)
}

// Request asks an executor to run a graph.
type Request struct {
	Graph     cache.Blob // the encoded graph, in the build cache
	Rebuild   bool       // run the roots again, and resolve their pulls' tags again
	Timestamp time.Time  // the earliest time that targets which run stamp their files with
}

// Events receives a run's progress. Its methods may be called from several goroutines.
type Events interface {
	// Status reports what is happening to node, such as "building" or "cached".
	Status(node graph.Ref, status string)
	// Output passes on what node's run() steps print, or printed when its cached result was made, to stream:
	// cache.StreamStdout or cache.StreamStderr. data is valid only during the call.
	Output(node graph.Ref, stream byte, data []byte)
}

// Result is what a root made. Exactly one of Image, Index and Files is set, unless the root is a task without
// output.
type Result struct {
	Node  graph.Ref
	Image string      // a pull's or layer's image ID, in the executor's store
	Index []Manifest  // an index's images, in order
	Files *cache.Blob // a task's files, as a tree
	Epoch int64       // when Files' files were stamped, in Unix seconds
}

// Manifest is an image of an index.
type Manifest struct {
	Image    string // its ID, in the executor's store
	Platform string // as platforms.Format gives it
}

// Local is the executor that runs in this process, with buildah, on the local container store and build cache.
// It opens them only once it needs them, so that commands which fail before building touch neither.
type Local struct {
	systemContext *types.SystemContext
	storeOptions  storage.StoreOptions
	store         storage.Store
	cache         *cache.Cache
}

// NewLocal returns a local executor that reaches registries as sys says. The caller must close it.
func NewLocal(sys *types.SystemContext) (*Local, error) {
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, err
	}
	return &Local{systemContext: sys, storeOptions: options}, nil
}

// CacheDir returns where the build cache is kept, which the client and a local executor share.
func CacheDir() (string, error) {
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		return "", err
	}
	return cacheDir(options), nil
}

// open opens the store and the build cache, unless they are open.
func (l *Local) open() error {
	if l.store != nil {
		return nil
	}
	c, err := cache.Open(cacheDir(l.storeOptions))
	if err != nil {
		return err
	}
	store, err := storage.GetStore(l.storeOptions)
	if err != nil {
		return err
	}
	l.cache, l.store = c, store
	return nil
}

// Close closes the store, if it was opened.
func (l *Local) Close() {
	if l.store != nil {
		l.store.Shutdown(false)
	}
}

// ExecPlatform returns the host's platform.
func (l *Local) ExecPlatform(context.Context) (imgspecv1.Platform, error) {
	return HostPlatform, nil
}

// Run runs the graph req names with buildah: its pulls first, then its layers, tasks and indexes in order. Each
// layer's and task's result is kept in the build cache, and reused instead of running the target again whenever
// a later build gives it the same inputs, unless it is a root and req.Rebuild is set.
func (l *Local) Run(ctx context.Context, req Request, ev Events) ([]Result, error) {
	if err := l.open(); err != nil {
		return nil, err
	}
	// Read the graph as a remote executor would, rather than share the client's, so that nothing reaches the
	// build but what the graph holds.
	data, err := l.cache.ReadBytes(req.Graph)
	if err != nil {
		return nil, fmt.Errorf("reading the build graph: %w", err)
	}
	g, err := graph.Decode(data)
	if err != nil {
		return nil, err
	}
	rebuild := map[graph.Ref]bool{}
	for _, ref := range g.Roots {
		rebuild[ref] = req.Rebuild
		// An index is built by building its manifests.
		if idx := g.Node(ref).Index; idx != nil {
			for _, m := range idx.Manifests {
				rebuild[m] = req.Rebuild
			}
		}
	}

	// Pull everything first, so a bad image name is reported before any slow target runs.
	images := map[graph.Ref]builtImage{
		graph.Scratch: {id: scratch, epoch: time.Unix(0, 0)}, // buildah starts an empty image from this name
	}
	pl := &puller{
		store:         l.store,
		cache:         l.cache,
		systemContext: l.systemContext,
		tags:          map[string]*tagRecord{},
		again:         map[string]bool{},
	}
	for i, n := range g.Nodes {
		// Every pull of a tag that one pulls again gets the image it names now, even if pulled before that one.
		if n.Pull != nil && rebuild[graph.Ref(i)] {
			pl.again[n.Pull.Image] = true
		}
	}
	for i, n := range g.Nodes {
		if n.Pull == nil {
			continue
		}
		if rebuild[graph.Ref(i)] {
			// The tag may name a newer image now.
			ev.Status(graph.Ref(i), "pulling again")
		}
		if images[graph.Ref(i)], err = pl.pull(ctx, n.Pull); err != nil {
			return nil, fmt.Errorf("%s: %w", n.Desc, err)
		}
	}

	conf, err := containersconfig.Default()
	if err != nil {
		return nil, err
	}
	capabilities, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return nil, err
	}
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return nil, err
	}

	r := targetRunner{
		graph:         g,
		store:         l.store,
		cache:         l.cache,
		systemContext: l.systemContext,
		timestamp:     req.Timestamp,
		events:        ev,
		capabilities:  capabilities,
		isolation:     isolation,
		images:        images,
		trees:         map[graph.Ref]setTree{},
		indexes:       map[graph.Ref][]Manifest{},
	}
	for i, n := range g.Nodes {
		switch {
		case n.Layer != nil, n.Task != nil:
			err = r.runCached(ctx, graph.Ref(i), rebuild[graph.Ref(i)])
		case n.Index != nil:
			err = r.runIndex(ctx, graph.Ref(i))
		}
		if err != nil {
			return nil, err
		}
	}

	var results []Result
	for _, ref := range g.Roots {
		res := Result{Node: ref}
		switch n := g.Node(ref); {
		case n.Pull != nil, n.Layer != nil:
			res.Image = r.images[ref].id
		case n.Index != nil:
			res.Index = r.indexes[ref]
		case n.Task != nil && n.Task.Output != nil:
			t := r.trees[ref]
			res.Files, res.Epoch = &t.tree, t.epoch.Unix()
		}
		results = append(results, res)
	}
	return results, nil
}

// runIndex makes the result of the index ref: its manifests' images, each checked to be for its platform.
func (r *targetRunner) runIndex(ctx context.Context, ref graph.Ref) error {
	n := r.graph.Node(ref)
	var manifests []Manifest
	for _, m := range n.Index.Manifests {
		id := r.images[m].id
		want := r.graph.Node(m).Platform()
		got, err := imagePlatform(ctx, r.store, id)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Desc, err)
		}
		if platforms.Format(got) != want {
			return fmt.Errorf("%s: the image of %s is for %s, not %s", n.Desc, r.graph.Node(m).Label,
				platforms.Format(got), want)
		}
		manifests = append(manifests, Manifest{Image: id, Platform: want})
	}
	r.indexes[ref] = manifests
	return nil
}

// Push copies the image or index of res to the registry dest.
func (l *Local) Push(ctx context.Context, res Result, dest reference.Named) (string, error) {
	ref, err := docker.NewReference(dest)
	if err != nil {
		return "", err
	}
	return l.Export(ctx, res, ref, l.systemContext)
}

// Export copies the image or index of res from the container store to dest.
func (l *Local) Export(ctx context.Context, res Result, dest types.ImageReference, sys *types.SystemContext) (
	string, error) {
	if err := l.open(); err != nil {
		return "", err
	}
	switch {
	case res.Index != nil:
		return l.exportIndex(ctx, res.Index, dest, sys)
	case res.Image != "":
		return l.exportImage(ctx, res.Image, dest, sys)
	}
	return "", errors.New("the target made no image")
}

// exportImage copies the image with ID id from the container store to dest, returning the ID it has
// there: the digest of its config, as written there, which may differ from the store's if its
// manifest is converted, as docker-archive does.
func (l *Local) exportImage(ctx context.Context, id string, dest types.ImageReference, sys *types.SystemContext) (
	string, error) {
	src, err := is.Transport.NewStoreReference(l.store, nil, id)
	if err != nil {
		return "", err
	}
	policyContext, err := l.policyContext()
	if err != nil {
		return "", err
	}
	defer policyContext.Destroy()

	written, err := imagecopy.Image(ctx, policyContext, dest, src, &imagecopy.Options{DestinationCtx: sys})
	if err != nil {
		return "", err
	}
	m, err := manifest.FromBlob(written, manifest.GuessMIMEType(written))
	if err != nil {
		return "", fmt.Errorf("reading the manifest written: %w", err)
	}
	return m.ConfigInfo().Digest.Encoded(), nil
}

// policyContext returns the signature policy to copy images under: the system's, as buildah follows when
// committing, but trusting local storage and layouts, where vamos has just put the images. The caller
// must destroy it.
func (l *Local) policyContext() (*signature.PolicyContext, error) {
	policy, err := signature.DefaultPolicy(l.systemContext)
	if err != nil {
		return nil, err
	}
	for _, transport := range []string{is.Transport.Name(), layout.Transport.Name()} {
		policy.Transports[transport] = signature.PolicyTransportScopes{
			"": {signature.NewPRInsecureAcceptAnything()},
		}
	}
	return signature.NewPolicyContext(policy)
}

// exportIndex copies the images of manifests from the container store to dest, as an index listing each with its
// platform, in order. It returns the digest of the index there.
func (l *Local) exportIndex(
	ctx context.Context, manifests []Manifest, dest types.ImageReference, sys *types.SystemContext,
) (string, error) {
	// containers/image copies an index, and its images, from a source that has one, so make one: an OCI
	// layout holding the images, and an index of them, which its index.json names.
	dir, err := os.MkdirTemp("", "vamos-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	policyContext, err := l.policyContext()
	if err != nil {
		return "", err
	}
	defer policyContext.Destroy()

	index := imgspecv1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: imgspecv1.MediaTypeImageIndex}
	for i, m := range manifests {
		src, err := is.Transport.NewStoreReference(l.store, nil, m.Image)
		if err != nil {
			return "", err
		}
		layoutRef, err := layout.NewReference(dir, fmt.Sprintf("manifest-%d", i))
		if err != nil {
			return "", err
		}
		written, err := imagecopy.Image(ctx, policyContext, layoutRef, src, &imagecopy.Options{
			ForceManifestMIMEType: imgspecv1.MediaTypeImageManifest,
		})
		if err != nil {
			return "", err
		}
		platform, err := graph.ParsePlatform(m.Platform)
		if err != nil {
			return "", err
		}
		index.Manifests = append(index.Manifests, imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageManifest,
			Digest:    digest.FromBytes(written),
			Size:      int64(len(written)),
			Platform:  &platform,
		})
	}
	if err := writeLayoutIndex(dir, index, "index"); err != nil {
		return "", err
	}

	src, err := layout.NewReference(dir, "index")
	if err != nil {
		return "", err
	}
	written, err := imagecopy.Image(ctx, policyContext, dest, src, &imagecopy.Options{
		DestinationCtx:     sys,
		ImageListSelection: imagecopy.CopyAllImages,
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(written).String(), nil
}

// eventWriter passes what is written to it on to the events of a node, as the output of one stream.
type eventWriter struct {
	events Events
	node   graph.Ref
	stream byte
}

func (w eventWriter) Write(data []byte) (int, error) {
	w.events.Output(w.node, w.stream, data)
	return len(data), nil
}
