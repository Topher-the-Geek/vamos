package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/transfer"

	"github.com/opencontainers/go-digest"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// submitPublish starts a job that publishes req's images, each to its repository under every tag,
// with the executor's credentials and registry settings: it uploads them one at a time, since they share this host's
// network, then, only if every upload succeeded, writes every tag, one at a time.
func (l *Local) submitPublish(req api.PublishRequest) (api.JobID, error) {
	repos, dests, err := sched.ParsePublication(req)
	if err != nil {
		return "", err
	}
	for _, image := range req.Images {
		if err := checkResult(image.Result); err != nil {
			return "", err
		}
	}
	return l.jobs.Start(false, func(ctx context.Context, ev sched.Events) api.Outcome {
		ctx, flush := sched.ForCall(ctx, req.Trace, ev)
		defer flush() // after the span ends
		ctx, span := tracing.StartTrack(ctx, "publish", "publish")
		if err := l.open(); err != nil {
			span.EndWith(err)
			return sched.Failed(err)
		}
		uploads := make([]remote.Uploaded, len(req.Images))
		for i, image := range req.Images {
			u, err := uploadResult(ctx, l.store, l.systemContext, image.Result, repos[i])
			if err != nil {
				span.EndWith(err)
				outcome := sched.Failed(err)
				outcome.Failure.Nodes = []graph.Ref{image.Result.Node}
				return outcome
			}
			uploads[i] = u
		}
		outcome := tagAll(ctx, l.systemContext, req.Images, dests, uploads)
		if outcome.Failure != nil {
			span.EndWith(errors.New(outcome.Failure.Message))
		} else {
			span.End()
		}
		return outcome
	})
}

// uploadResult uploads the image or index of res to the repository repo, by digest, with no tag,
// reading it from store, if store holds it, or else from its locations, with the registry settings sys,
// and returns what it wrote.
// If ctx is traced, the upload is a span.
func uploadResult(ctx context.Context, store storage.Store, sys *types.SystemContext, res api.Result,
	repo reference.Named) (_ remote.Uploaded, err error) {
	ctx, span := tracing.Start(ctx, "upload to "+repo.String())
	defer func() { span.EndWith(err) }()
	written, err := sendResult(ctx, store, sys, res, func(
		ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference,
		options *imagecopy.Options,
	) ([]byte, error) {
		return transfer.Upload(ctx, policyContext, repo, src, options)
	})
	if err != nil {
		return remote.Uploaded{}, fmt.Errorf("uploading to %s: %w", repo, err)
	}
	u := remote.Uploaded{Manifest: written}
	if u.Image, u.Index, err = describeWritten(res, written); err != nil {
		return remote.Uploaded{}, fmt.Errorf("uploading to %s: %w", repo, err)
	}
	return u, nil
}

// sender writes an image or index from src, as transfer.Push and transfer.Upload do, returning the manifest written,
// with the options sendResult gives it.
type sender func(
	ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference, options *imagecopy.Options,
) ([]byte, error)

// sendResult sends the image or index of res with send, reading each image from store, if store holds it,
// or else from its locations, with the registry settings sys, and returns the manifest written.
// An index is assembled in a layout first, listing each image with its platform, in order.
func sendResult(ctx context.Context, store storage.Store, sys *types.SystemContext, res api.Result, send sender) (
	_ []byte, err error) {
	policyContext, err := transfer.Policy(sys)
	if err != nil {
		return nil, err
	}
	defer policyContext.Destroy()
	switch {
	case res.Index != nil:
		// dir is assigned, not declared, so that the cleanup below reports to the named result, not a copy.
		var dir string
		if dir, err = os.MkdirTemp("", "vamos-index-"); err != nil {
			return nil, err
		}
		defer func() {
			if rerr := os.RemoveAll(dir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("removing the copied blobs: %w", rerr))
			}
		}()
		sources := make([]types.ImageReference, len(res.Index))
		for i, m := range res.Index {
			if sources[i], err = sourceOf(ctx, store, m.Image, m.Locations); err != nil {
				return nil, err
			}
		}
		src, _, err := layoutIndexFrom(ctx, policyContext, dir, res.Index, sources, sys)
		if err != nil {
			return nil, err
		}
		return send(ctx, policyContext, src, &imagecopy.Options{
			SourceCtx:          sys,
			DestinationCtx:     sys,
			ImageListSelection: imagecopy.CopyAllImages,
		})
	case res.Image != "":
		src, err := sourceOf(ctx, store, res.Image, res.Locations)
		if err != nil {
			return nil, err
		}
		return send(ctx, policyContext, src, &imagecopy.Options{SourceCtx: sys, DestinationCtx: sys})
	}
	return nil, errors.New("the target made no image")
}

// tagAll writes, with the registry settings sys, each tag of dests, the destinations of each of images in turn,
// as ParsePublication returns them, naming what uploads says was uploaded for that image, attempting every one even
// if another fails, and returns the publish job's outcome: what each destination names and whether it was written
// and read back, and a failure naming each tag that wasn't, and each that was, if any wasn't.
func tagAll(ctx context.Context, sys *types.SystemContext, images []api.PublishImage,
	dests [][]reference.NamedTagged, uploads []remote.Uploaded) api.Outcome {
	published := &api.Published{}
	var failedNode *graph.Ref
	for i, image := range images {
		for _, dest := range dests[i] {
			d := api.Destination{
				Repository: reference.TrimNamed(dest).String(), Tag: dest.Tag(),
				Image: uploads[i].Image, Index: uploads[i].Index,
			}
			if err := transfer.Tag(ctx, dest, uploads[i].Manifest, sys); err != nil {
				d.Error = brief(err.Error())
				if failedNode == nil {
					failedNode = &image.Result.Node
				}
			} else {
				d.Written = true
			}
			published.Destinations = append(published.Destinations, d)
		}
	}
	outcome := api.Outcome{Published: published}
	if msg := published.Shortfall(); msg != "" {
		outcome.Cancelled = errors.Is(ctx.Err(), context.Canceled)
		node := images[0].Result.Node
		if failedNode != nil {
			node = *failedNode
		}
		outcome.Failure = &api.Failure{Nodes: []graph.Ref{node}, Message: msg, Hint: api.PublishHint}
	}
	return outcome
}

// maxTagError is the most of a tag's error that a publication's outcome keeps: enough to say why, while every
// destination's, twice over, as the outcome and its failure name them, stays well within what a worker reports.
const maxTagError = 1 << 10

// brief returns msg, cut to maxTagError bytes, at a character's start, if it is longer, saying so.
func brief(msg string) string {
	if len(msg) <= maxTagError {
		return msg
	}
	cut := maxTagError
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + " … (cut short)"
}

// describeWritten returns what written, the manifest of res's image or index as a push or upload wrote it, names:
// an image's ID there, the digest of its config, which may differ from the store's if its manifest is converted,
// or an index's digest.
func describeWritten(res api.Result, written []byte) (image, index string, _ error) {
	if res.Index != nil {
		return "", digest.FromBytes(written).String(), nil
	}
	id, err := imageIDOf(written)
	return id, "", err
}

// imageIDOf returns the ID of the image whose manifest, as written, is written: the digest of its config.
func imageIDOf(written []byte) (string, error) {
	m, err := manifest.FromBlob(written, manifest.GuessMIMEType(written))
	if err != nil {
		return "", fmt.Errorf("reading the manifest written: %w", err)
	}
	return m.ConfigInfo().Digest.Encoded(), nil
}
