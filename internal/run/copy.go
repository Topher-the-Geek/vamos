package run

import (
	"context"
	"errors"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
)

// progressInterval is how often containers/image would report a blob's progress. It sends no events at all
// unless this is set, but a trace has no use for the reads between a blob's start and end, so it is long enough
// that there are none.
const progressInterval = time.Hour

// tracedCopy copies the image or index src to dest, as imagecopy.Image does, returning the manifest written. If
// ctx is traced, the copy is a span, holding a span for each blob copied, and an instant for each skipped.
func tracedCopy(ctx context.Context, policyContext *signature.PolicyContext, dest, src types.ImageReference,
	options *imagecopy.Options) ([]byte, error) {
	if !tracing.Enabled(ctx) {
		return imagecopy.Image(ctx, policyContext, dest, src, options)
	}
	ctx, span := tracing.Start(ctx, "copy to "+transports.ImageName(dest), "source", transports.ImageName(src))
	opts := *options
	progress := make(chan types.ProgressProperties)
	opts.Progress, opts.ProgressInterval = progress, progressInterval
	blobs := newBlobTracker(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			blobs.event(p)
		}
	}()
	written, err := imagecopy.Image(ctx, policyContext, dest, src, &opts)
	close(progress) // copy.Image sends nothing once it returns
	<-done
	blobs.close(ctx, err)
	if err == nil {
		span.Set(writtenArgs(written)...)
	}
	span.EndWith(err)
	return written, err
}

// writtenArgs returns what a trace records of the manifest a copy wrote: its digest, and the blobs it lists, as
// the destination has them, or the manifests of an index.
func writtenArgs(written []byte) []any {
	args := []any{"manifest", digest.FromBytes(written).String()}
	mimeType := manifest.GuessMIMEType(written)
	if manifest.MIMETypeIsMultiImage(mimeType) {
		list, err := manifest.ListFromBlob(written, mimeType)
		if err != nil {
			return args
		}
		var manifests []string
		for _, d := range list.Instances() {
			manifests = append(manifests, d.String())
		}
		return append(args, "manifests", manifests)
	}
	m, err := manifest.FromBlob(written, mimeType)
	if err != nil {
		return args
	}
	type blob struct {
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
		MediaType string `json:"media type,omitempty"`
	}
	blobs := []blob{{Digest: m.ConfigInfo().Digest.String(), Size: m.ConfigInfo().Size,
		MediaType: m.ConfigInfo().MediaType}}
	for _, l := range m.LayerInfos() {
		blobs = append(blobs, blob{Digest: l.Digest.String(), Size: l.Size, MediaType: l.MediaType})
	}
	return append(args, "blobs", blobs)
}

// blobTracker makes spans of the blobs of a copy from the events containers/image sends. They don't pair up: a
// blob skipped, as already at the destination, is reported with no start, and a blob whose copy fails or is
// cancelled has no end.
type blobTracker struct {
	ctx  context.Context
	open map[digest.Digest][]*tracing.Active // spans of blobs being copied, by source digest, earliest first
}

// newBlobTracker returns a tracker of blobs copied, as part of ctx's span.
func newBlobTracker(ctx context.Context) *blobTracker {
	return &blobTracker{ctx: ctx, open: map[digest.Digest][]*tracing.Active{}}
}

// event records what p reports. Its Artifact is the source's blob, which may differ from the destination's, as
// when a layer is compressed, and its Offset, once done, is how many bytes the destination took.
func (b *blobTracker) event(p types.ProgressProperties) {
	d := p.Artifact.Digest
	switch p.Event {
	case types.ProgressEventNewArtifact:
		span := tracing.StartAsync(b.ctx, "blob "+shortDigest(d), sourceArgs(p.Artifact)...)
		b.open[d] = append(b.open[d], span)
	case types.ProgressEventDone:
		spans := b.open[d]
		if len(spans) == 0 {
			return
		}
		spans[0].Set("bytes sent", p.Offset)
		spans[0].End()
		if b.open[d] = spans[1:]; len(b.open[d]) == 0 {
			delete(b.open, d)
		}
	case types.ProgressEventSkipped:
		tracing.Instant(b.ctx, "skipped blob "+shortDigest(d), append(sourceArgs(p.Artifact),
			"already present", true)...)
	}
}

// close ends the spans of blobs whose copy didn't finish, once the copy has returned err, as failed or cancelled.
func (b *blobTracker) close(ctx context.Context, err error) {
	outcome := "failed"
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		outcome = "cancelled"
	case err == nil:
		outcome = "unfinished" // containers/image never reported its end
	}
	for d, spans := range b.open {
		for _, span := range spans {
			span.Set("outcome", outcome)
			span.EndWith(err)
		}
		delete(b.open, d)
	}
}

// sourceArgs returns what a trace records of the source's blob info: its digest, its size if known, and its media
// type if given.
func sourceArgs(info types.BlobInfo) []any {
	args := []any{"source digest", info.Digest.String()}
	if info.Size >= 0 {
		args = append(args, "source size", info.Size)
	}
	if info.MediaType != "" {
		args = append(args, "media type", info.MediaType)
	}
	return args
}

// shortDigest returns d as a span's name shows it: its algorithm and the first 12 digits.
func shortDigest(d digest.Digest) string {
	if len(d.Encoded()) <= 12 {
		return d.String()
	}
	return d.Algorithm().String() + ":" + d.Encoded()[:12]
}
