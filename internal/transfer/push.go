package transfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	containersconfig "go.podman.io/common/pkg/config"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// Retries returns how to retry a registry operation after an error that may not happen again,
// such as a connection reset: as containers.conf's retry and retry_delay say, as for buildah and podman,
// backing off exponentially from a second unless retry_delay is set.
// Tests replace it, so that they don't depend on the host's containers.conf.
var Retries = func() (*retry.Options, error) {
	conf, err := containersconfig.Default()
	if err != nil {
		return nil, err
	}
	options := &retry.Options{MaxRetry: int(conf.Engine.Retry)}
	if conf.Engine.RetryDelay != "" {
		if options.Delay, err = time.ParseDuration(conf.Engine.RetryDelay); err != nil {
			return nil, fmt.Errorf("containers.conf's retry_delay: %w", err)
		}
	}
	return options, nil
}

// readBackTimeout limits reading a pushed tag back, which is small,
// so that a registry that stops answering is reported rather than waited for.
const readBackTimeout = time.Minute

// Push copies the image or index src to the tag dest at a registry, as sys reaches it,
// retrying transient failures as Retries says, then reads the tag back, retrying likewise,
// and checks that it names the manifest written, and returns that manifest.
func Push(
	ctx context.Context, policyContext *signature.PolicyContext, dest reference.NamedTagged, src types.ImageReference,
	sys *types.SystemContext, options *imagecopy.Options,
) ([]byte, error) {
	ref, err := docker.NewReference(dest)
	if err != nil {
		return nil, err
	}
	retries, err := Retries()
	if err != nil {
		return nil, err
	}
	written, err := copyRetrying(ctx, policyContext, ref, src, options, retries)
	if err != nil {
		return nil, err
	}
	want, err := manifest.Digest(written)
	if err != nil {
		return nil, fmt.Errorf("reading the manifest written: %w", err)
	}
	if err := checkTag(ctx, dest, ref, sys, want, "the push", "pushed", retries); err != nil {
		return nil, err
	}
	return written, nil
}

// Upload copies the image or index src to the repository repo at a registry, by its manifest's digest,
// with no tag, retrying transient failures as Retries says, and returns the manifest written.
// Since it names nothing a user pulls by tag, an upload that fails, or is repeated, changes no tag;
// Tag then names what it wrote.
// It leaves out src's signatures, as of an image pulled from a registry that signs them,
// which a destination with no tag can't store.
func Upload(
	ctx context.Context, policyContext *signature.PolicyContext, repo reference.Named, src types.ImageReference,
	options *imagecopy.Options,
) ([]byte, error) {
	ref, err := docker.NewReferenceUnknownDigest(repo)
	if err != nil {
		return nil, err
	}
	retries, err := Retries()
	if err != nil {
		return nil, err
	}
	opts := *options
	opts.RemoveSignatures = true
	return copyRetrying(ctx, policyContext, uploadReference{ref}, src, &opts, retries)
}

// uploadReference is a reference to a repository to upload to by digest, whose destinations are uploadDestinations.
type uploadReference struct{ types.ImageReference }

// NewImageDestination returns the reference's destination as an uploadDestination.
func (r uploadReference) NewImageDestination(ctx context.Context, sys *types.SystemContext) (
	types.ImageDestination, error) {
	dest, err := r.ImageReference.NewImageDestination(ctx, sys)
	if err != nil {
		return nil, err
	}
	return uploadDestination{dest}, nil
}

// uploadDestination is a registry destination with no tag that accepts an empty set of signatures for an index,
// which is all Upload gives it.
// containers/image, in v5.41.2 and still in v5.42.0, stores an index's signatures even when there are none,
// and a destination with no tag refuses them, since it never learns the index's digest,
// so an index can't otherwise be uploaded by digest.
// Hiding the destination's private interface from the copy costs what vamos doesn't use:
// reusing a layer in a compression the options require, partial uploads and sigstore signatures.
type uploadDestination struct{ types.ImageDestination }

// PutSignatures stores signatures, as the destination does, and does nothing when there are none.
func (d uploadDestination) PutSignatures(
	ctx context.Context, signatures [][]byte, instanceDigest *digest.Digest,
) error {
	if len(signatures) == 0 {
		return nil
	}
	return d.ImageDestination.PutSignatures(ctx, signatures, instanceDigest)
}

// Tag writes the manifest uploaded, which Upload returned, to the tag dest in the repository Upload wrote it to,
// as sys reaches it: every blob and every manifest an index lists are already there, so this is all it sends.
// It writes to the registry itself, never to one of the mirrors registries.conf may configure for pulling from it,
// and reads nothing from either but the tag it wrote.
// It retries transient failures as Retries says, then reads the tag back, retrying likewise,
// and checks that it names the manifest uploaded.
// If ctx is traced, tagging is a span.
func Tag(ctx context.Context, dest reference.NamedTagged, uploaded []byte, sys *types.SystemContext) (err error) {
	want, err := manifest.Digest(uploaded)
	if err != nil {
		return fmt.Errorf("reading the manifest uploaded: %w", err)
	}
	ctx, span := tracing.Start(ctx, "tag "+dest.String(), "manifest", want.String())
	defer func() { span.EndWith(err) }()
	ref, err := docker.NewReference(dest)
	if err != nil {
		return err
	}
	retries, err := Retries()
	if err != nil {
		return err
	}
	err = retry.IfNecessary(ctx, func() error {
		return putManifest(ctx, ref, uploaded, sys)
	}, retries)
	if err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return checkTag(ctx, dest, ref, sys, want, "tagging", "uploaded", retries)
}

// putManifest writes the manifest m to ref, a tag at a registry, as sys reaches it.
func putManifest(ctx context.Context, ref types.ImageReference, m []byte, sys *types.SystemContext) (err error) {
	dest, err := ref.NewImageDestination(ctx, sys)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dest.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	if err := dest.PutManifest(ctx, m, nil); err != nil {
		return err
	}
	return dest.Commit(ctx, nil)
}

// copyRetrying copies src to dest, as Image does, retrying transient failures as retries says,
// and returns the manifest written.
func copyRetrying(
	ctx context.Context, policyContext *signature.PolicyContext, dest, src types.ImageReference,
	options *imagecopy.Options, retries *retry.Options,
) ([]byte, error) {
	var written []byte
	err := retry.IfNecessary(ctx, func() error {
		var err error
		written, err = Image(ctx, policyContext, dest, src, options)
		return err
	}, retries)
	return written, err
}

// checkTag reads the tag dest, at ref, back, retrying as retries says,
// and checks that it names the manifest want; a mismatch is reported as found after the write that after names,
// of the manifest that what describes.
func checkTag(
	ctx context.Context, dest reference.NamedTagged, ref types.ImageReference, sys *types.SystemContext,
	want digest.Digest, after, what string, retries *retry.Options,
) error {
	var got digest.Digest
	err := retry.IfNecessary(ctx, func() error {
		var err error
		got, err = readBack(ctx, ref, sys)
		return err
	}, retries)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", dest, err)
	}
	if got != want {
		return fmt.Errorf("after %s, %s names manifest %s, not the one %s, %s", after, dest, got, what, want)
	}
	return nil
}

// readBack fetches the digest of the manifest that ref names at its registry: the registry itself,
// where the push wrote, never one of the mirrors registries.conf may configure for pulling from it,
// which may not hold the tag yet, or may hold an older one.
func readBack(ctx context.Context, ref types.ImageReference, sys *types.SystemContext) (digest.Digest, error) {
	ctx, cancel := context.WithTimeout(ctx, readBackTimeout)
	defer cancel()
	return docker.GetDigest(ctx, sys, ref)
}
