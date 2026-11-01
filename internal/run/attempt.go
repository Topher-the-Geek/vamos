package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/Topher-the-Geek/vamos/internal/transfer"
	"go.podman.io/buildah"
	"go.podman.io/buildah/pkg/parse"
	containersconfig "go.podman.io/common/pkg/config"

	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/signature"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// AttemptInput is what a worker gives the subprocess that runs an attempt, on its standard input:
// the assignment, and where to reach the coordinator's content store and publish to.
type AttemptInput struct {
	Attempt         string               `json:"attempt"`
	Request         *sched.Request       `json:"request,omitempty"`
	Push            *remote.PushWork     `json:"push,omitempty"`
	Coordinator     string               `json:"coordinator"`                // the URL of its content store
	CacheRepository string               `json:"cache_repository,omitempty"` // where built layers are pushed
	PullThrough     map[string]string    `json:"pull_through,omitempty"`     // upstream registry to its cache
	Trace           *tracing.CallContext `json:"trace,omitempty"`            // if set, the attempt is traced
}

// AttemptLine is one line the subprocess prints: what the attempt printed, spans of its trace, or its report,
// which is the last line.
type AttemptLine struct {
	Output *OutputLine           `json:"output,omitempty"`
	Spans  []tracing.Span        `json:"spans,omitempty"`
	Report *remote.AttemptReport `json:"report,omitempty"`
}

// OutputLine is one write of an attempt's run() steps.
type OutputLine struct {
	Stream byte   `json:"stream"`
	Data   []byte `json:"data"`
}

// MaxLineSize is the most bytes a line the subprocess prints may have: a report holds a few records at most,
// and output is written as it comes.
const MaxLineSize = 16 << 20

// lineWriter prints AttemptLines, one at a time, from any goroutine.
type lineWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// write prints line.
func (w *lineWriter) write(line AttemptLine) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enc.Encode(line) // a failure to print can't be reported anywhere but by ending
}

// Output prints what the attempt's run() steps printed, as sched.Output takes it; data is encoded before it returns,
// so it isn't copied.
func (w *lineWriter) Output(stream byte, data []byte) {
	w.write(AttemptLine{Output: &OutputLine{Stream: stream, Data: data}})
}

// Attempt runs the attempt that in describes, in this process, which a worker started in a cgroup of its own,
// printing what it does to out as AttemptLines, the report last, and building in the container store that options
// describe. Its content store is the coordinator's, over HTTP; its container is named for the attempt.
func Attempt(ctx context.Context, in io.Reader, out io.Writer, options storage.StoreOptions) error {
	var input AttemptInput
	if err := json.NewDecoder(in).Decode(&input); err != nil {
		return fmt.Errorf("reading the attempt: %w", err)
	}
	w := &lineWriter{enc: json.NewEncoder(out)}
	report := attempt(ctx, input, w, options)
	w.write(AttemptLine{Report: &report})
	return nil
}

// attempt runs input, printing output and spans to w, and returns its report.
func attempt(ctx context.Context, input AttemptInput, w *lineWriter, options storage.StoreOptions) (
	report remote.AttemptReport) {
	failed := func(err error) remote.AttemptReport {
		return remote.AttemptReport{Failure: sched.FailureOf(err)}
	}
	content, err := remote.NewClient(input.Coordinator)
	if err != nil {
		return failed(err)
	}
	defer content.Close()
	sys, err := SystemContext()
	if err != nil {
		return failed(err)
	}
	conf, err := containersconfig.Default()
	if err != nil {
		return failed(err)
	}
	capabilities, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return failed(err)
	}
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return failed(err)
	}
	store, err := storage.GetStore(options)
	if err != nil {
		return failed(fmt.Errorf("opening the container store: %w", err))
	}
	defer store.Shutdown(false)
	if input.Trace != nil {
		ctx = tracing.ForCall(ctx, input.Trace, func(s tracing.Span) {
			w.write(AttemptLine{Spans: []tracing.Span{s}})
		}, "worker")
	}
	r := &localRunner{
		store:           store,
		content:         content,
		systemContext:   sys,
		capabilities:    capabilities,
		isolation:       isolation,
		attempt:         input.Attempt,
		cacheRepository: input.CacheRepository,
		pullThrough:     input.PullThrough,
	}
	switch {
	case input.Request != nil:
		rep, err := r.Run(ctx, *input.Request, w)
		if err != nil {
			return failed(err)
		}
		return remote.AttemptReport{Report: rep}
	case input.Push != nil:
		rep, err := pushWork(ctx, store, sys, *input.Push)
		if err != nil {
			return failed(err)
		}
		return rep
	}
	return failed(errors.New("the attempt asks for nothing"))
}

// RemoveAttempt removes the container that the attempt id left in the store that options describe, and its mounts,
// as a worker does after a kill, or on starting after a crash.
func RemoveAttempt(ctx context.Context, options storage.StoreOptions, id string) error {
	store, err := storage.GetStore(options)
	if err != nil {
		return fmt.Errorf("opening the container store: %w", err)
	}
	defer store.Shutdown(false)
	return removeAttempt(store, id)
}

// removeAttempt removes the attempt id's container from store: through buildah, which unmounts it, or, for a
// container buildah never finished recording, as a kill between its creation and its state's write leaves it,
// directly.
func removeAttempt(store storage.Store, id string) error {
	name := attemptPrefix + id
	builder, err := buildah.OpenBuilder(store, name)
	switch {
	case errors.Is(err, storage.ErrContainerUnknown):
		return nil
	case errors.Is(err, os.ErrNotExist):
		container, err := store.Container(name)
		if errors.Is(err, storage.ErrContainerUnknown) {
			return nil
		} else if err != nil {
			return fmt.Errorf("finding the attempt's container: %w", err)
		}
		if _, err := store.Unmount(container.ID, true); err != nil && !errors.Is(err, storage.ErrLayerNotMounted) {
			return fmt.Errorf("unmounting the attempt's container: %w", err)
		}
		if err := store.DeleteContainer(container.ID); err != nil {
			return fmt.Errorf("removing the attempt's container: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("opening the attempt's container: %w", err)
	}
	var errs []error
	if err := builder.Unmount(); err != nil && !errors.Is(err, storage.ErrLayerNotMounted) {
		errs = append(errs, fmt.Errorf("unmounting the attempt's container: %w", err))
	}
	if err := builder.Delete(); err != nil {
		errs = append(errs, fmt.Errorf("removing the attempt's container: %w", err))
	}
	return errors.Join(errs...)
}

// LeftoverAttempts returns the IDs of the attempts whose containers remain in the store that options describe,
// as a worker that crashed left them, so that the worker removes them on starting.
func LeftoverAttempts(options storage.StoreOptions) ([]string, error) {
	store, err := storage.GetStore(options)
	if err != nil {
		return nil, fmt.Errorf("opening the container store: %w", err)
	}
	defer store.Shutdown(false)
	containers, err := store.Containers()
	if err != nil {
		return nil, fmt.Errorf("listing the containers: %w", err)
	}
	var ids []string
	for _, c := range containers {
		for _, name := range c.Names {
			if id, ok := strings.CutPrefix(name, attemptPrefix); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// pushWork makes the write to a registry that work asks for, as its mode says, with the registry settings sys,
// reading images from store, if it holds them, or else from their locations, and reports it.
func pushWork(ctx context.Context, store storage.Store, sys *types.SystemContext, work remote.PushWork) (
	remote.AttemptReport, error) {
	switch work.Mode {
	case remote.ModePush:
		dest, err := sched.ParseDestination(work.Destination)
		if err != nil {
			return remote.AttemptReport{}, err
		}
		pushed, err := pushResult(ctx, store, sys, work.Result, dest)
		if err != nil {
			return remote.AttemptReport{}, err
		}
		return remote.AttemptReport{Pushed: &pushed}, nil
	case remote.ModeUpload:
		repo, err := sched.ParseRepository(work.Repository)
		if err != nil {
			return remote.AttemptReport{}, err
		}
		u, err := uploadResult(ctx, store, sys, work.Result, repo)
		if err != nil {
			return remote.AttemptReport{}, err
		}
		return remote.AttemptReport{Uploaded: &u}, nil
	case remote.ModeTag:
		if work.Tag == nil || len(work.Tag.Uploads) != len(work.Tag.Images) {
			return remote.AttemptReport{}, errors.New("the tagging names an upload for each image, but doesn't")
		}
		_, dests, err := sched.ParsePublication(api.PublishRequest{Images: work.Tag.Images, Tags: work.Tag.Tags})
		if err != nil {
			return remote.AttemptReport{}, err
		}
		outcome := tagAll(ctx, sys, work.Tag.Images, dests, work.Tag.Uploads)
		return remote.AttemptReport{Tagged: &outcome}, nil
	}
	return remote.AttemptReport{}, fmt.Errorf("the attempt asks for a write of mode %q, which this worker can't make",
		work.Mode)
}

// pushResult pushes the image or index of res to dest, from the store, if it holds the image, or else from its
// locations, and returns what it made there.
func pushResult(ctx context.Context, store storage.Store, sys *types.SystemContext, res api.Result,
	dest reference.NamedTagged) (api.Pushed, error) {
	written, err := sendResult(ctx, store, sys, res, func(
		ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference,
		options *imagecopy.Options,
	) ([]byte, error) {
		return transfer.Push(ctx, policyContext, dest, src, sys, options)
	})
	if err != nil {
		return api.Pushed{}, err
	}
	image, index, err := describeWritten(res, written)
	return api.Pushed{Image: image, Index: index}, err
}

// sourceOf returns a reference to read the image with ID id from: the store, if it holds it, and otherwise the
// first of locations at a registry.
func sourceOf(ctx context.Context, store storage.Store, id string, locations []cache.Location) (
	types.ImageReference, error) {
	var inStore error
	if store != nil {
		storeID, _, err := locateImage(ctx, store, id)
		if err == nil {
			return is.Transport.NewStoreReference(store, nil, storeID)
		}
		inStore = err
	}
	for _, l := range locations {
		if l.Kind != cache.LocationRegistry {
			continue
		}
		named, err := reference.ParseNamed(l.Reference)
		if err != nil {
			return nil, fmt.Errorf("location %q: %w", l.Reference, err)
		}
		return docker.NewReference(named)
	}
	if inStore != nil {
		return nil, fmt.Errorf("image %s is in no registry, and not in this store: %w", shortID(id), inStore)
	}
	return nil, fmt.Errorf("image %s is in no registry, and not in this store", shortID(id))
}
