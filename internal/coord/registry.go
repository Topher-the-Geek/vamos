package coord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"

	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"
)

// Registry checks images at registries, with the coordinator's own registry settings.
type Registry struct {
	Sys *types.SystemContext
}

// registryTimeout limits a check, which fetches one manifest.
const registryTimeout = time.Minute

// CheckImage reports why the image with ID id isn't at location, if it isn't: the registry has no such manifest,
// or holds another image under that reference. A registry that can't be reached is an error.
func (r *Registry) CheckImage(ctx context.Context, id string, location cache.Location) (problem, err error) {
	named, err := reference.ParseNamed(location.Reference)
	if err != nil {
		return fmt.Errorf("its location %q isn't a reference: %w", location.Reference, err), nil
	}
	ref, err := docker.NewReference(named)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	src, err := ref.NewImageSource(ctx, r.Sys)
	if err != nil {
		if unavailable(err) {
			return fmt.Errorf("the registry has no %s", location.Reference), nil
		}
		return nil, err
	}
	defer src.Close()
	data, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		if unavailable(err) {
			return fmt.Errorf("the registry has no %s", location.Reference), nil
		}
		return nil, err
	}
	m, err := manifest.FromBlob(data, mimeType)
	if err != nil {
		return fmt.Errorf("the manifest at %s can't be read: %w", location.Reference, err), nil
	}
	if got := m.ConfigInfo().Digest.Encoded(); got != id {
		return fmt.Errorf("%s is image %s, not %s", location.Reference, got[:12], id[:12]), nil
	}
	return nil, nil
}

// ClearRepository deletes every tag of repository, so that the cache repository holds nothing a pruned record
// could name, and returns how many tags it removed; the registry must allow deletion, and the credentials must
// permit it, so a refusal is an error, where a repository the registry doesn't have holds nothing to remove.
// Deleting a manifest removes every tag of it, so a tag that is gone by the time its turn comes counts as removed.
func (r *Registry) ClearRepository(ctx context.Context, repository string) (int, error) {
	named, err := reference.ParseNamed(repository)
	if err != nil {
		return 0, fmt.Errorf("the cache repository %q isn't a reference: %w", repository, err)
	}
	repo := reference.TrimNamed(named)
	// The transport wants a tagged reference even to list a repository's tags; the tag is ignored.
	anyTag, err := reference.WithTag(repo, "latest")
	if err != nil {
		return 0, err
	}
	ref, err := docker.NewReference(anyTag)
	if err != nil {
		return 0, err
	}
	tags, err := docker.GetRepositoryTags(ctx, r.Sys, ref)
	if err != nil {
		if missing(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing the tags of %s: %w", repository, err)
	}
	for i, tag := range tags {
		tagged, err := reference.WithTag(repo, tag)
		if err != nil {
			return i, fmt.Errorf("tag %q of %s: %w", tag, repository, err)
		}
		tagRef, err := docker.NewReference(tagged)
		if err != nil {
			return i, err
		}
		if err := tagRef.DeleteImage(ctx, r.Sys); err != nil && !missing(err) {
			return i, fmt.Errorf("deleting %s: %w", tagged, err)
		}
	}
	return len(tags), nil
}

// missing reports whether err says the registry has no such repository, manifest or tag: an answer that there is
// nothing, as opposed to a refusal or an unreachable registry.
func missing(err error) bool {
	var status docker.UnexpectedHTTPStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return true
	}
	var registryErr errcode.Error
	if errors.As(err, &registryErr) {
		switch registryErr.Code {
		case v2.ErrorCodeNameUnknown, v2.ErrorCodeManifestUnknown:
			return true
		}
	}
	// Deleting a tag that is gone fails on the lookup of its manifest, which the transport reports in words alone.
	return err != nil && strings.Contains(err.Error(), "Image may not exist")
}

// unavailable reports whether err says the registry has no such image, or won't show it without credentials,
// as opposed to the registry being unreachable.
func unavailable(err error) bool {
	var unauthorized docker.ErrUnauthorizedForCredentials
	if errors.As(err, &unauthorized) {
		return true
	}
	var registryErr errcode.Error
	if !errors.As(err, &registryErr) {
		return false
	}
	switch registryErr.Code {
	case errcode.ErrorCodeDenied, errcode.ErrorCodeUnauthorized, v2.ErrorCodeNameUnknown, v2.ErrorCodeManifestUnknown:
		return true
	}
	return false
}
