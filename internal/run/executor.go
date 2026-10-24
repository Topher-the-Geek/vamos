package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/parse"
	containersconfig "go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// Local is the executor that runs in this process, with buildah, on a container store and the build cache beside it.
// It opens them only once it needs them, so that commands which fail before building touch neither,
// and the container store only to run a job, so that capturing sources doesn't need it.
// Its jobs run in goroutines of their own and are kept, with their events, until it is closed;
// Close cancels those still running and waits for them.
type Local struct {
	systemContext *types.SystemContext
	storeOptions  storage.StoreOptions
	instance      string // identifies this executor, in its capabilities
	jobs          sched.Jobs

	mu        sync.Mutex // guards opening the store and cache
	store     storage.Store
	directory *cache.Directory   // beside the container store, holding its lock while open
	content   cache.ContentStore // the directory
	records   cache.RecordStore  // the directory
	runner    *localRunner       // executes actions on the container store, and checks cached results against it
	sharing   *sched.Sharing     // runs actions through the runner, once for every job that needs the same one
}

// NewLocal returns a local executor on the container store that options describe, such as DefaultStoreOptions
// gives, which reaches registries as its environment says: registries.conf, and $REGISTRY_AUTH_FILE for credentials.
// The client's registry settings never reach it.
// The caller must close it.
func NewLocal(options storage.StoreOptions) (*Local, error) {
	sys, err := SystemContext()
	if err != nil {
		return nil, err
	}
	var id [8]byte
	rand.Read(id[:])
	return &Local{systemContext: sys, storeOptions: options, instance: hex.EncodeToString(id[:])}, nil
}

// SystemContext returns the settings for reaching registries that this process's environment gives,
// as transfer.SystemContext does: the local executor and the client, which share the environment, each build their
// own.
func SystemContext() (*types.SystemContext, error) {
	return transfer.SystemContext()
}

// Isolation returns how this process runs commands, such as "chroot", as buildah's environment says.
func Isolation() string {
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return ""
	}
	return isolation.String()
}

// Version returns the version of vamos, as the release build stamped it or the Go module that built it records,
// or "" if that is unknown.
func Version() string {
	return vamosVersion()
}

// RetainEvents makes the executor retain n events of each job for watchers, rather than the default,
// so that tests can see gaps without long jobs.
// It is for tests, before any job is submitted.
func (l *Local) RetainEvents(n int) {
	l.jobs.RetainEvents(n)
}

// DefaultStoreOptions returns the container store that buildah and podman use here, as containers/storage finds it.
func DefaultStoreOptions() (storage.StoreOptions, error) {
	return storage.DefaultStoreOptions()
}

// CacheDir returns where the build cache beside the container store that options describe is kept,
// which the client and a local executor share.
func CacheDir(options storage.StoreOptions) string {
	return cacheDir(options)
}

// openContent opens the build cache, unless it is open; l.mu must be held.
func (l *Local) openContent() error {
	if l.directory != nil {
		return nil
	}
	d, err := cache.OpenDirectory(cacheDir(l.storeOptions))
	if err != nil {
		return err
	}
	l.directory, l.content, l.records = d, d, d
	return nil
}

// open opens the build cache and the container store, unless they are open.
// A failure to open the container store leaves the build cache open, as ContentStore may have opened it;
// Close releases it.
func (l *Local) open() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openContent(); err != nil {
		return err
	}
	if l.store != nil {
		return nil
	}
	conf, err := containersconfig.Default()
	if err != nil {
		return err
	}
	capabilities, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return err
	}
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return err
	}
	store, err := storage.GetStore(l.storeOptions)
	if err != nil {
		return err
	}
	l.runner = &localRunner{
		store:         store,
		content:       l.directory,
		systemContext: l.systemContext,
		capabilities:  capabilities,
		isolation:     isolation,
	}
	l.sharing = sched.NewSharing(l.runner, l.directory, l.directory)
	l.store = store
	return nil
}

// ContentStore returns the content store the executor reads submitted content from,
// which a client building locally captures sources into and delivers from.
// It opens the build cache alone, not the container store,
// so that capturing sources, and refusing them, needs nothing that only building does.
func (l *Local) ContentStore() (cache.ContentStore, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openContent(); err != nil {
		return nil, err
	}
	return l.content, nil
}

// maxGraphSize is the most bytes a graph may have, so that a corrupt or hostile one is refused rather than decoded.
const maxGraphSize = 64 << 20

// Close shuts the executor down: it cancels the jobs still running and waits for them to end,
// so that their containers and pending files are removed, then shuts the container store down
// and releases the build cache's lock, reporting what failed.
func (l *Local) Close() error {
	// Every attempt has ended once its jobs have: the last job to detach from one waits for it.
	l.jobs.Stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	var errs []error
	if l.store != nil {
		// Shutdown reports a layer used if any in the store is mounted, by this process or another sharing the store,
		// such as a second vamos building at the same time. This executor's jobs have removed their own containers,
		// and reported a failure to, so a layer still mounted is another's, and that is no failure.
		// force must stay false: it would unmount every mounted layer in the store, under another's running build.
		if _, err := l.store.Shutdown(false); err != nil && !errors.Is(err, storage.ErrLayerUsedByContainer) {
			errs = append(errs, fmt.Errorf("shutting down the container store: %w", err))
		}
		l.store = nil
	}
	if l.directory != nil {
		if err := l.directory.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the build cache: %w", err))
		}
		l.directory, l.content, l.records, l.runner, l.sharing = nil, nil, nil, nil, nil
	}
	return errors.Join(errs...)
}

// graphVersions are the graph versions the executor runs.
var graphVersions = []int{graph.Version}

// Capabilities reports the host's platform, the one execution platform it offers, the graph versions this version
// of vamos runs, how commands are isolated, and the time.
func (l *Local) Capabilities(context.Context) (api.Capabilities, error) {
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return api.Capabilities{}, err
	}
	return api.Capabilities{
		Platforms:     []imgspecv1.Platform{HostPlatform},
		GraphVersions: graphVersions,
		Isolation:     isolation.String(),
		Version:       vamosVersion(),
		Clock:         time.Now(),
		Instance:      l.instance,
	}.Clone(), nil
}

// checkExecPlatform refuses a graph configured for an executor on platform, if this one isn't.
func checkExecPlatform(platform string) error {
	if want := platforms.Format(HostPlatform); platform != want {
		return fmt.Errorf("the graph was configured for an executor on %s, but this one runs on %s", platform, want)
	}
	return nil
}

// stampedVersion is the release's version, such as v0.1.0, which the release build sets with
// -ldflags "-X github.com/Topher-the-Geek/vamos/internal/run.stampedVersion=v0.1.0";
// the Dockerfile builds outside the repository's git metadata, so the build info can't record it.
var stampedVersion string

// vamosVersion returns the version of vamos, as the release build stamped it or the Go module that built it records,
// or "" if that is unknown.
func vamosVersion() string {
	if stampedVersion != "" {
		return stampedVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "(devel)" {
		return ""
	}
	return info.Main.Version
}

// Submit starts a job for req: a graph run, a push or an export.
// A graph of a version it can't run, or configured for another platform, is refused before anything else;
// so is a push to a destination that isn't a registry reference with a tag.
func (l *Local) Submit(ctx context.Context, req api.Request) (api.JobID, error) {
	req = req.Clone() // the job runs after Submit returns, and the caller's request is its own again
	kinds := 0
	for _, set := range []bool{req.Graph != nil, req.Push != nil, req.Export != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return "", fmt.Errorf("the request asks for %d jobs, want 1", kinds)
	}
	switch {
	case req.Push != nil:
		if err := checkResult(req.Push.Result); err != nil {
			return "", err
		}
		return l.submitPush(*req.Push)
	case req.Export != nil:
		if err := checkResult(req.Export.Result); err != nil {
			return "", err
		}
		return l.submitExport(*req.Export)
	}
	g := req.Graph
	if !slices.Contains(graphVersions, g.Version) {
		return "", fmt.Errorf("graph version %d isn't supported; this executor runs version %d", g.Version,
			graph.Version)
	}
	if err := checkExecPlatform(platforms.Format(platforms.Normalize(g.Platform))); err != nil {
		return "", err
	}
	return l.jobs.Start(false, func(ctx context.Context, ev sched.Events) api.Outcome {
		results, err := l.run(ctx, *g, ev)
		if err != nil {
			return sched.Failed(err)
		}
		return api.Outcome{Results: results}
	})
}

// Watch delivers the events of the job id from position from to fn, as api.Executor describes.
func (l *Local) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	j, err := l.jobs.Lookup(id)
	if err != nil {
		return err
	}
	return j.Watch(ctx, from, fn)
}

// Cancel asks the job id to stop; it does so between commands, which buildah can't interrupt.
func (l *Local) Cancel(_ context.Context, id api.JobID) error {
	j, err := l.jobs.Lookup(id)
	if err != nil {
		return err
	}
	j.Cancel()
	return nil
}

// Release lets go of the job id; the local executor's jobs live as long as the command, so it only checks the job.
func (l *Local) Release(_ context.Context, id api.JobID) error {
	j, err := l.jobs.Lookup(id)
	if err != nil {
		return err
	}
	j.Release()
	return nil
}

// Result returns the outcome of the job id, or ErrRunning.
func (l *Local) Result(_ context.Context, id api.JobID) (api.Outcome, error) {
	j, err := l.jobs.Lookup(id)
	if err != nil {
		return api.Outcome{}, err
	}
	return j.Result()
}

// Prune starts a job that removes every cached result from the container store and the build cache.
// It is refused while any other job of this executor runs, and no job is accepted while it runs;
// it fails with cache.ErrLocked while another process holds the build cache open.
func (l *Local) Prune(context.Context) (api.JobID, error) {
	return l.jobs.Start(true, func(ctx context.Context, ev sched.Events) api.Outcome {
		pruned, err := l.prune(ctx)
		if err != nil {
			return sched.Failed(err)
		}
		return api.Outcome{Pruned: &pruned}
	})
}

// run runs the graph req names with buildah: its pulls first, then its layers, tasks and indexes in order.
// Each layer's and task's result is kept in the build cache,
// and reused instead of running the target again whenever a later build gives it the same inputs,
// unless it is a root and req.Rebuild is set.
func (l *Local) run(ctx context.Context, req api.GraphRequest, ev sched.Events) (_ []api.Result, err error) {
	ctx, flush := sched.ForCall(ctx, req.Trace, ev)
	defer flush() // after the span ends
	ctx, span := tracing.StartTrack(ctx, "run", "run", "graph", req.Graph.Digest.String(), "rebuild", req.Rebuild)
	defer func() { span.EndWith(err) }()
	content, err := l.ContentStore()
	if err != nil {
		return nil, err
	}
	// Read the graph as a remote executor would, rather than share the client's,
	// so that nothing reaches the build but what the graph holds.
	// A graph that is missing or corrupt fails the job, naming the blob; the client may submit it again.
	// It is read before the container store is opened, which a graph that can't run doesn't need.
	data, err := content.Read(ctx, req.Graph, maxGraphSize)
	if err != nil {
		return nil, fmt.Errorf("reading the build graph: %w", err)
	}
	g, err := graph.Decode(data)
	if err != nil {
		return nil, err
	}
	if err := checkExecPlatform(g.ExecPlatform); err != nil {
		return nil, err
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	// Targets run one at a time: parallel local scheduling is a separate decision.
	// Emulation needs no permission locally, as it never has; whether it should is decided before remote execution
	// is released, and the switch is optIn.
	b := sched.Backend{Content: l.content, Records: l.records, Checker: l.runner, Runner: l.sharing}
	opts := sched.Options{
		Rebuild: req.Rebuild, Timestamp: req.Timestamp, Emulate: req.Emulate, Bound: 1, Secrets: req.Secrets,
	}
	return sched.New(g, b, ev, opts).Run(ctx)
}

// checkResult refuses a result to push or export whose image IDs aren't image IDs, as Result.Image gives them,
// so that one that names nothing this executor could have made is refused before a job starts.
func checkResult(res api.Result) error {
	if res.Image != "" {
		if err := digest.SHA256.Validate(res.Image); err != nil {
			return fmt.Errorf("the result's image ID %q: %w", res.Image, err)
		}
	}
	for i, m := range res.Index {
		if err := digest.SHA256.Validate(m.Image); err != nil {
			return fmt.Errorf("the image ID %q of the result's manifest %d: %w", m.Image, i, err)
		}
	}
	return nil
}

// shortID abbreviates the image ID id for messages, as container tools do, leaving one too short as it is.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// submitPush starts a job that pushes req.Result's image or index to the registry req.Destination names,
// with the executor's credentials and registry settings.
// The destination must be a full reference with a tag and no digest: the client resolves short names,
// so that both sides mean the same registry.
func (l *Local) submitPush(req api.PushRequest) (api.JobID, error) {
	dest, err := sched.ParseDestination(req.Destination)
	if err != nil {
		return "", err
	}
	return l.jobs.Start(false, func(ctx context.Context, ev sched.Events) api.Outcome {
		ctx, flush := sched.ForCall(ctx, req.Trace, ev)
		defer flush() // after the span ends
		ctx, span := tracing.StartTrack(ctx, "push", "push to "+dest.String())
		pushed, err := l.push(ctx, req.Result, dest)
		span.EndWith(err)
		if err != nil {
			outcome := sched.Failed(err)
			outcome.Failure.Nodes = []graph.Ref{req.Result.Node}
			return outcome
		}
		return api.Outcome{Pushed: &pushed}
	})
}

// push copies the image or index of res from the container store to the tag dest at its registry,
// and returns what it made there.
func (l *Local) push(ctx context.Context, res api.Result, dest reference.NamedTagged) (api.Pushed, error) {
	if err := l.open(); err != nil {
		return api.Pushed{}, err
	}
	switch {
	case res.Index != nil:
		d, err := l.pushIndex(ctx, res.Index, dest)
		return api.Pushed{Index: d}, err
	case res.Image != "":
		id, err := l.pushImage(ctx, res.Image, dest)
		return api.Pushed{Image: id}, err
	}
	return api.Pushed{}, errors.New("the target made no image")
}

// pushImage copies the image with ID id from the container store to dest, returning the ID it has there:
// the digest of its config, as written there, which may differ from the store's if its manifest is converted.
func (l *Local) pushImage(ctx context.Context, id string, dest reference.NamedTagged) (string, error) {
	src, err := l.imageReference(ctx, id)
	if err != nil {
		return "", err
	}
	return pushImageFrom(ctx, l.systemContext, src, dest)
}

// pushImageFrom copies the image at src to dest, with the settings sys, returning the ID it has there:
// the digest of its config, as written there, which may differ from the source's if its manifest is converted.
func pushImageFrom(ctx context.Context, sys *types.SystemContext, src types.ImageReference,
	dest reference.NamedTagged) (string, error) {
	policyContext, err := transfer.Policy(sys)
	if err != nil {
		return "", err
	}
	defer policyContext.Destroy()
	written, err := transfer.Push(ctx, policyContext, dest, src, sys, &imagecopy.Options{DestinationCtx: sys})
	if err != nil {
		return "", err
	}
	m, err := manifest.FromBlob(written, manifest.GuessMIMEType(written))
	if err != nil {
		return "", fmt.Errorf("reading the manifest written: %w", err)
	}
	return m.ConfigInfo().Digest.Encoded(), nil
}

// pushIndex copies the images of manifests from the container store to dest,
// as an index listing each with its platform, in order, through a layout, and returns the index's digest there.
func (l *Local) pushIndex(ctx context.Context, manifests []api.Manifest, dest reference.NamedTagged) (
	_ string, err error) {
	dir, err := os.MkdirTemp("", "vamos-index-")
	if err != nil {
		return "", err
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the copied blobs: %w", rerr))
		}
	}()
	policyContext, err := transfer.Policy(l.systemContext)
	if err != nil {
		return "", err
	}
	defer policyContext.Destroy()
	src, _, err := l.layoutIndex(ctx, policyContext, dir, manifests)
	if err != nil {
		return "", err
	}
	written, err := transfer.Push(ctx, policyContext, dest, src, l.systemContext, &imagecopy.Options{
		DestinationCtx:     l.systemContext,
		ImageListSelection: imagecopy.CopyAllImages,
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(written).String(), nil
}

// imageReference returns the reference of the image with ID id in the container store,
// located by its name in the cache repository and referred to by the store's ID found, which can't move.
func (l *Local) imageReference(ctx context.Context, id string) (types.ImageReference, error) {
	storeID, _, err := locateImage(ctx, l.store, id)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", shortID(id), err)
	}
	return is.Transport.NewStoreReference(l.store, nil, storeID)
}

// submitExport starts a job that publishes req.Result's content to the content store, for a client to deliver.
func (l *Local) submitExport(req api.ExportRequest) (api.JobID, error) {
	return l.jobs.Start(false, func(ctx context.Context, ev sched.Events) api.Outcome {
		ctx, flush := sched.ForCall(ctx, req.Trace, ev)
		defer flush() // after the span ends
		ctx, span := tracing.StartTrack(ctx, "export", "export to the content store")
		exported, err := l.export(ctx, req.Result)
		span.EndWith(err)
		if err != nil {
			outcome := sched.Failed(err)
			outcome.Failure.Nodes = []graph.Ref{req.Result.Node}
			return outcome
		}
		return api.Outcome{Exported: exported}
	})
}

// export publishes the content of res to the content store: a task's tree is there already;
// an image's manifest, config and layers, or an index's and its images', are copied out of the container store
// through a layout, and imported.
// Only an export copies image blobs out of the container store; a task's export doesn't open it.
func (l *Local) export(ctx context.Context, res api.Result) (_ *api.Exported, err error) {
	if res.Files != nil {
		content, err := l.ContentStore()
		if err != nil {
			return nil, err
		}
		if err := cache.CheckTree(ctx, content, *res.Files); err != nil {
			return nil, fmt.Errorf("the task's files can't be delivered from the content store: %w", err)
		}
		return &api.Exported{Files: res.Files, Epoch: res.Epoch}, nil
	}
	if res.Image == "" && res.Index == nil {
		return nil, errors.New("the target made no image and no files")
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "vamos-export-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the copied blobs: %w", rerr))
		}
	}()
	policyContext, err := transfer.Policy(l.systemContext)
	if err != nil {
		return nil, err
	}
	defer policyContext.Destroy()
	var written []byte
	mediaType := imgspecv1.MediaTypeImageManifest
	if res.Index != nil {
		if _, written, err = l.layoutIndex(ctx, policyContext, dir, res.Index); err != nil {
			return nil, err
		}
		mediaType = imgspecv1.MediaTypeImageIndex
	} else {
		if written, err = l.layoutImage(ctx, policyContext, dir, res.Image, "image"); err != nil {
			return nil, err
		}
	}
	blobs, err := transfer.LayoutBlobs(dir)
	if err != nil {
		return nil, err
	}
	manifestBlob := cache.Blob{Digest: digest.FromBytes(written), Size: int64(len(written))}
	exported := &api.Exported{Manifest: manifestBlob, MediaType: mediaType}
	_, span := tracing.Start(ctx, "publish blobs", "manifest", manifestBlob.Digest.String(), "blobs", len(blobs))
	defer func() { span.EndWith(err) }()
	for _, b := range blobs {
		if err := l.importBlob(ctx, dir, b); err != nil {
			return nil, err
		}
		if b != manifestBlob {
			exported.Blobs = append(exported.Blobs, b)
		}
	}
	return exported, nil
}

// importBlob imports blob b from the layout in dir into the content store, whether or not it holds one by that
// digest: Has tells a blob of the right size from none, not a corrupt one, and an export must deliver.
func (l *Local) importBlob(ctx context.Context, dir string, b cache.Blob) error {
	f, err := os.Open(transfer.LayoutBlobPath(dir, b))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := l.content.Import(ctx, b, f); err != nil {
		return fmt.Errorf("publishing blob %s: %w", b.Digest, err)
	}
	return nil
}

// layoutImage copies the image with ID id from the container store into the OCI layout in dir, as ref,
// with an OCI manifest, and returns the manifest written.
func (l *Local) layoutImage(
	ctx context.Context, policyContext *signature.PolicyContext, dir, id, ref string,
) ([]byte, error) {
	src, err := l.imageReference(ctx, id)
	if err != nil {
		return nil, err
	}
	layoutRef, err := layout.NewReference(dir, ref)
	if err != nil {
		return nil, err
	}
	return transfer.Image(ctx, policyContext, layoutRef, src, &imagecopy.Options{
		ForceManifestMIMEType: imgspecv1.MediaTypeImageManifest,
	})
}

// layoutIndex copies the images of manifests from the container store into the OCI layout in dir,
// and makes an index listing each with its platform, in order, which the layout's index.json names as "index",
// and returns a reference to it and the index's bytes.
// containers/image copies an index, and its images, only from a source that has one, which this makes.
func (l *Local) layoutIndex(
	ctx context.Context, policyContext *signature.PolicyContext, dir string, manifests []api.Manifest,
) (types.ImageReference, []byte, error) {
	sources := make([]types.ImageReference, len(manifests))
	for i, m := range manifests {
		var err error
		if sources[i], err = l.imageReference(ctx, m.Image); err != nil {
			return nil, nil, err
		}
	}
	return layoutIndexFrom(ctx, policyContext, dir, manifests, sources, l.systemContext)
}

// layoutIndexFrom copies the images of manifests, each from its source, into the OCI layout in dir, and makes an
// index listing each with its platform, in order, as transfer.LayoutIndex does, returning a reference to it and its
// bytes.
func layoutIndexFrom(
	ctx context.Context, policyContext *signature.PolicyContext, dir string, manifests []api.Manifest,
	sources []types.ImageReference, sys *types.SystemContext,
) (types.ImageReference, []byte, error) {
	members := make([]transfer.IndexMember, len(manifests))
	for i, m := range manifests {
		platform, err := graph.ParsePlatform(m.Platform)
		if err != nil {
			return nil, nil, err
		}
		members[i] = transfer.IndexMember{Platform: platform, Source: sources[i]}
	}
	return transfer.LayoutIndex(ctx, policyContext, dir, members, sys)
}
