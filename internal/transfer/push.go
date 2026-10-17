package transfer

import (
	"context"
	"fmt"
	"time"

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
	var written []byte
	err = retry.IfNecessary(ctx, func() error {
		written, err = Image(ctx, policyContext, ref, src, options)
		return err
	}, retries)
	if err != nil {
		return nil, err
	}
	want, err := manifest.Digest(written)
	if err != nil {
		return nil, fmt.Errorf("reading the manifest written: %w", err)
	}
	var got digest.Digest
	err = retry.IfNecessary(ctx, func() error {
		got, err = readBack(ctx, ref, sys)
		return err
	}, retries)
	if err != nil {
		return nil, fmt.Errorf("reading %s back: %w", dest, err)
	}
	if got != want {
		return nil, fmt.Errorf("after the push, %s names manifest %s, not the one pushed, %s", dest, got, want)
	}
	return written, nil
}

// readBack fetches the digest of the manifest that ref names at its registry: the registry itself,
// where the push wrote, never one of the mirrors registries.conf may configure for pulling from it,
// which may not hold the tag yet, or may hold an older one.
func readBack(ctx context.Context, ref types.ImageReference, sys *types.SystemContext) (digest.Digest, error) {
	ctx, cancel := context.WithTimeout(ctx, readBackTimeout)
	defer cancel()
	return docker.GetDigest(ctx, sys, ref)
}
