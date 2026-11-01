package finish

import (
	"context"
	"fmt"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/transfer"

	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// Publication is an image or index for the client to publish, and the repository to publish it to.
type Publication struct {
	Image      Image
	Repository reference.Named // with no tag or digest
}

// Publish publishes each of images to its repository under every one of tags, with the client's credentials and
// registry settings, sys, as an executor's publish job does: it uploads each by digest, one at a time, since they
// share this host's network, then, only once every upload has succeeded, writes every tag, one at a time,
// attempting each even if another fails, and reading each back.
// A failed upload returns its error, naming the repository, having written no tag.
// Otherwise it returns every destination, in order, and, if any tag isn't known to be written, an error saying which
// weren't and which were; an interruption while tagging is that error, wrapping ctx's.
// If ctx is traced, each upload and each tag is a span.
func Publish(ctx context.Context, images []Publication, tags []string, sys *types.SystemContext) (
	[]api.Destination, error) {
	type uploaded struct {
		manifest     []byte
		image, index string
	}
	uploads := make([]uploaded, len(images))
	for i, p := range images {
		written, err := upload(ctx, p, sys)
		if err != nil {
			return nil, fmt.Errorf("uploading to %s: %w", p.Repository, err)
		}
		id, err := describeWritten(written)
		if err != nil {
			return nil, fmt.Errorf("uploading to %s: %w", p.Repository, err)
		}
		u := uploaded{manifest: written, image: id}
		if manifest.MIMETypeIsMultiImage(manifest.GuessMIMEType(written)) {
			u = uploaded{manifest: written, index: id}
		}
		uploads[i] = u
	}
	var published api.Published
	for i, p := range images {
		for _, tag := range tags {
			d := api.Destination{Repository: p.Repository.Name(), Tag: tag, Image: uploads[i].image,
				Index: uploads[i].index}
			dest, err := reference.WithTag(p.Repository, tag)
			if err == nil {
				err = transfer.Tag(ctx, dest, uploads[i].manifest, sys)
			}
			if err != nil {
				d.Error = err.Error()
			} else {
				d.Written = true
			}
			published.Destinations = append(published.Destinations, d)
		}
	}
	if msg := published.Shortfall(); msg != "" {
		if err := ctx.Err(); err != nil {
			return published.Destinations, fmt.Errorf("%w: %s", err, msg)
		}
		return published.Destinations, fmt.Errorf("%s; %s", msg, api.PublishHint)
	}
	return published.Destinations, nil
}

// upload uploads p's image to its repository by digest, and returns the manifest written.
// If ctx is traced, the upload is a span.
func upload(ctx context.Context, p Publication, sys *types.SystemContext) (_ []byte, err error) {
	ctx, span := tracing.Start(ctx, "upload to "+p.Repository.String())
	defer func() { span.EndWith(err) }()
	return sendImage(ctx, p.Image, sys, func(
		ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference,
		options *imagecopy.Options,
	) ([]byte, error) {
		return transfer.Upload(ctx, policyContext, p.Repository, src, options)
	})
}
