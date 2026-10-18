package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
)

// progressInterval is how often containers/image would report a blob's progress.
// It sends no events at all unless this is set,
// but a trace has no use for the reads between a blob's start and end, so it is long enough that there are none.
const progressInterval = time.Hour

// Image copies the image or index src to dest, as imagecopy.Image does, returning the manifest written.
// If ctx is traced, the copy is a span, holding a span for each blob copied, and an instant for each skipped.
func Image(ctx context.Context, policyContext *signature.PolicyContext, dest, src types.ImageReference,
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

// writtenArgs returns what a trace records of the manifest a copy wrote: its digest,
// and the blobs it lists, as the destination has them, or the manifests of an index.
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

// blobTracker makes spans of the blobs of a copy from the events containers/image sends.
// They don't pair up: a blob skipped, as already at the destination, is reported with no start,
// and a blob whose copy fails or is cancelled has no end.
// event is confined to Image's progress goroutine.
// close runs only after that goroutine finishes, so open needs no mutex.
// Do not use the tracker concurrently.
type blobTracker struct {
	ctx  context.Context
	open map[digest.Digest][]*tracing.Active // spans of blobs being copied, by source digest, earliest first
}

// newBlobTracker returns a tracker of blobs copied, as part of ctx's span.
func newBlobTracker(ctx context.Context) *blobTracker {
	return &blobTracker{ctx: ctx, open: map[digest.Digest][]*tracing.Active{}}
}

// event records what p reports.
// Its Artifact is the source's blob, which may differ from the destination's, as when a layer is compressed,
// and its Offset, once done, is how many bytes the destination took.
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

// sourceArgs returns what a trace records of the source's blob info:
// its digest, its size if known, and its media type if given.
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

// Policy returns the signature policy to copy images under: the system's, as buildah follows when committing,
// but trusting local storage and layouts, where vamos has just put the images.
// The storage transport is named rather than imported, since this package must not reach the container store;
// a client never uses it, so trusting it there changes nothing.
// The caller must destroy it.
func Policy(sys *types.SystemContext) (*signature.PolicyContext, error) {
	policy, err := signature.DefaultPolicy(sys)
	if err != nil {
		return nil, err
	}
	for _, transport := range []string{"containers-storage", layout.Transport.Name()} {
		policy.Transports[transport] = signature.PolicyTransportScopes{
			"": {signature.NewPRInsecureAcceptAnything()},
		}
	}
	return signature.NewPolicyContext(policy)
}

// WriteLayoutIndex stores index as a blob of the OCI layout in dir,
// and makes the layout's index.json name it, as ref, alone, returning the index's bytes as stored.
func WriteLayoutIndex(dir string, index imgspecv1.Index, ref string) ([]byte, error) {
	data, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	d := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded()), data, 0o644); err != nil {
		return nil, err
	}
	return data, writeIndexJSON(dir, imgspecv1.Descriptor{
		MediaType: imgspecv1.MediaTypeImageIndex,
		Digest:    d,
		Size:      int64(len(data)),
	}, ref)
}

// writeIndexJSON makes the layout in dir's index.json name what desc describes, as ref, alone.
func writeIndexJSON(dir string, desc imgspecv1.Descriptor, ref string) error {
	desc.Annotations = map[string]string{imgspecv1.AnnotationRefName: ref}
	top := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{desc},
	}
	data, err := json.Marshal(top)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644)
}

// IndexMember is one image of an index to assemble: where to copy it from, and the platform the index lists it for.
type IndexMember struct {
	Platform imgspecv1.Platform
	Source   types.ImageReference
}

// LayoutIndex copies each member's image, read with the settings sys, into the OCI layout in dir, and makes an index
// listing each with its platform, in order, which the layout's index.json names as "index", and returns a reference
// to it and the index's bytes.
// containers/image copies an index, and its images, only from a source that has one, which this makes.
func LayoutIndex(
	ctx context.Context, policyContext *signature.PolicyContext, dir string, members []IndexMember,
	sys *types.SystemContext,
) (types.ImageReference, []byte, error) {
	index := imgspecv1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: imgspecv1.MediaTypeImageIndex}
	for i, m := range members {
		layoutRef, err := layout.NewReference(dir, fmt.Sprintf("manifest-%d", i))
		if err != nil {
			return nil, nil, err
		}
		written, err := Image(ctx, policyContext, layoutRef, m.Source, &imagecopy.Options{
			SourceCtx:             sys,
			ForceManifestMIMEType: imgspecv1.MediaTypeImageManifest,
		})
		if err != nil {
			return nil, nil, err
		}
		platform := m.Platform
		index.Manifests = append(index.Manifests, imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageManifest,
			Digest:    digest.FromBytes(written),
			Size:      int64(len(written)),
			Platform:  &platform,
		})
	}
	data, err := WriteLayoutIndex(dir, index, "index")
	if err != nil {
		return nil, nil, err
	}
	ref, err := layout.NewReference(dir, "index")
	return ref, data, err
}

// LayoutBlobs returns the blobs that the OCI layout in dir holds, by reading its blobs directory.
func LayoutBlobs(dir string) ([]cache.Blob, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		return nil, err
	}
	var blobs []cache.Blob
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		b := cache.Blob{Digest: digest.NewDigestFromEncoded(digest.SHA256, e.Name()), Size: info.Size()}
		if err := b.Validate(); err != nil {
			return nil, err
		}
		blobs = append(blobs, b)
	}
	return blobs, nil
}

// LayoutBlobPath returns where the OCI layout in dir keeps blob b.
func LayoutBlobPath(dir string, b cache.Blob) string {
	return filepath.Join(dir, "blobs", b.Digest.Algorithm().String(), b.Digest.Encoded())
}

// Layout writes an OCI layout in dir, which must exist and be empty, holding the blobs from store,
// whose index.json names the manifest, of mediaType, as "image", and returns a reference to it.
// A blob that is missing or corrupt in store fails the layout.
func Layout(
	ctx context.Context, store cache.ContentStore, dir string, manifest cache.Blob, mediaType string,
	blobs []cache.Blob,
) (types.ImageReference, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return nil, err
	}
	version := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), version, 0o644); err != nil {
		return nil, err
	}
	for _, b := range append([]cache.Blob{manifest}, blobs...) {
		if err := copyBlob(ctx, store, b, LayoutBlobPath(dir, b)); err != nil {
			return nil, err
		}
	}
	if err := writeIndexJSON(dir, imgspecv1.Descriptor{
		MediaType: mediaType,
		Digest:    manifest.Digest,
		Size:      manifest.Size,
	}, "image"); err != nil {
		return nil, err
	}
	return layout.NewReference(dir, "image")
}

// copyBlob writes blob b from store to the file at path, verified as it is read.
func copyBlob(ctx context.Context, store cache.ContentStore, b cache.Blob, path string) error {
	r, err := store.Get(ctx, b)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("reading blob %s from the content store: %w", b.Digest, err)
	}
	return f.Close()
}

// SystemContext returns the settings for reaching registries that this process's environment gives:
// containers/image doesn't read $REGISTRY_AUTH_FILE itself; podman and buildah pass it on.
// The local executor and the client, which share the environment, each build their own; a worker and a coordinator
// build theirs from their own environments, so no credential crosses between them.
func SystemContext() (*types.SystemContext, error) {
	sys := &types.SystemContext{}
	if path := os.Getenv("REGISTRY_AUTH_FILE"); path != "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("REGISTRY_AUTH_FILE: %w", err)
		}
		sys.AuthFilePath = path
	}
	return sys, nil
}
