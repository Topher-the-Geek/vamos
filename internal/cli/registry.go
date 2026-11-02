package cli

import (
	"context"
	"time"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// Registry is what pinning a pull asks of the registry of its image, from the client:
// which tags its repository has, and what one of them names now.
// Config.Registry replaces it, as scenarios do; otherwise the client asks the image's own registry.
type Registry interface {
	// Tags lists the tags of the repository repo, every page of them.
	Tags(ctx context.Context, repo reference.Named) ([]string, error)
	// Digest returns the digest of the manifest that ref names now, an index or an image.
	Digest(ctx context.Context, ref reference.NamedTagged) (digest.Digest, error)
}

// upstreamRegistry is the Registry that the client asks with its own credentials and --tls-verify, through sys:
// the image's own registry, never a mirror or pull-through cache, which may hold a stale answer about a tag.
type upstreamRegistry struct {
	sys *types.SystemContext
}

// Timeouts for asking a registry, so that one that stops answering is reported rather than waited for:
// a tag's digest is a small request, and a list of tags a few pages of them.
const (
	digestTimeout  = time.Minute
	tagListTimeout = 5 * time.Minute
)

// Tags lists the tags of repo at its registry, following every page of the list.
func (r upstreamRegistry) Tags(ctx context.Context, repo reference.Named) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, tagListTimeout)
	defer cancel()
	ref, err := docker.NewReference(reference.TagNameOnly(reference.TrimNamed(repo)))
	if err != nil {
		return nil, err
	}
	return docker.GetRepositoryTags(ctx, r.sys, ref)
}

// Digest asks ref's registry for the digest of the manifest its tag names, with HEAD, which Docker Hub doesn't count
// as a pull; a digest that ref carries too is ignored.
func (r upstreamRegistry) Digest(ctx context.Context, ref reference.NamedTagged) (digest.Digest, error) {
	ctx, cancel := context.WithTimeout(ctx, digestTimeout)
	defer cancel()
	tagged, err := reference.WithTag(reference.TrimNamed(ref), ref.Tag())
	if err != nil {
		return "", err
	}
	dockerRef, err := docker.NewReference(tagged)
	if err != nil {
		return "", err
	}
	return docker.GetDigest(ctx, r.sys, dockerRef)
}

// registry returns the Registry that pinning asks: Config's, if it gave one, and otherwise the upstream registry,
// with the client's settings.
func (inv *invocation) registry() (Registry, error) {
	if inv.app.registry != nil {
		return inv.app.registry, nil
	}
	sys, err := inv.systemContext()
	if err != nil {
		return nil, err
	}
	return upstreamRegistry{sys}, nil
}
