package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	containersconfig "go.podman.io/common/pkg/config"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// tagRecord is what a pull()'s tag named when vamos last resolved it, kept in the build cache so that later
// builds pull the same images, without reaching the registry, until --rebuild resolves it again.
type tagRecord struct {
	Digest    digest.Digest            `json:"digest"`              // of the manifest it named: an index or an image
	MIMEType  string                   `json:"mime_type,omitempty"` // the manifest's
	Manifest  []byte                   `json:"manifest,omitempty"`  // the manifest itself, a few KB
	Instances map[string]digest.Digest `json:"instances,omitempty"` // each platform's image, by platform, once chosen
}

// tagRecordOf returns the record of the tag ref, or nil if there is none.
func tagRecordOf(c *cache.Cache, ref reference.NamedTagged) (*tagRecord, error) {
	data, err := c.Tag(ref.String())
	if err != nil || data == nil {
		return nil, err
	}
	var record tagRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Digest == "" {
		return nil, nil // as good as none: the tag is resolved again
	}
	return &record, nil
}

// setTagRecord records what the tag ref names, replacing any earlier record.
func setTagRecord(c *cache.Cache, ref reference.NamedTagged, record *tagRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return c.SetTag(ref.String(), data)
}

// puller pulls the images of a build's pulls, each tag resolved once in the build, so that one tag gives every
// platform's image from the same index, even if the tag moves meanwhile.
type puller struct {
	store         storage.Store
	cache         *cache.Cache
	systemContext *types.SystemContext
	tags          map[string]*tagRecord // resolved in this build, by tag
	again         map[string]bool       // tags to resolve again, rather than as the build cache records them
}

// tracedPull pulls the image of n, the pull ref, as a span of ctx's trace, on a track of its own.
func (pl *puller) tracedPull(ctx context.Context, ref graph.Ref, n *graph.Node) (_ builtImage, err error) {
	ctx, span := tracing.StartTrack(ctx, n.Desc, n.Desc, "image", n.Pull.Image, "platform", n.Pull.Platform)
	defer func() { span.EndWith(err) }()
	img, err := pl.pull(ctx, n.Pull)
	if err == nil {
		span.Set("image ID", img.id, "epoch", img.epoch.UTC().Format(time.RFC3339))
		span.Produce(graph.FlowKey(ref))
	}
	return img, err
}

// pull fetches the image of p, for its platform, into the store, unless it is there already.
func (pl *puller) pull(ctx context.Context, p *graph.Pull) (builtImage, error) {
	image := p.Image
	named, err := reference.ParseNamed(image)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	tagged, ok := named.(reference.NamedTagged)
	if !ok {
		return builtImage{}, fmt.Errorf("image %q has no tag", image)
	}
	platform, err := graph.ParsePlatform(p.Platform)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	instance, err := pl.instance(ctx, tagged, platform)
	if err != nil {
		return builtImage{}, pullError(image, err)
	}
	digested, err := reference.WithDigest(reference.TrimNamed(tagged), instance)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	tracing.Current(ctx).Set("instance", instance.String())
	// Look for the image pulled before, by its manifest's digest, since copying it asks the registry about an
	// image even if the store has it. A pulled image is named by digest, since a tag can name only one
	// platform's image in the store, but one pulled before by its tag may have only that name.
	var img *storage.Image
	images, err := pl.store.ImagesByDigest(instance)
	if err == nil && len(images) > 0 {
		img = images[0]
		tracing.Current(ctx).Set("already present", true)
	} else if err == nil || errors.Is(err, storage.ErrImageUnknown) {
		img, err = pl.fetch(ctx, digested, platform)
		if err != nil {
			// With the tag's record cached, this is the first request to reach the registry.
			return builtImage{}, pullError(image, err)
		}
	}
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	id := img.ID
	got, err := imagePlatform(ctx, pl.store, id)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	if !platforms.NewMatcher(platform).Match(got) {
		return builtImage{}, fmt.Errorf("image %q is for %s, not %s, and its tag names that image alone, "+
			"not an index of images for several platforms", image, platforms.Format(got), platforms.Format(platform))
	}
	// Files built on the image are stamped with its creation time: a time that is the same on
	// every machine, and that moves forward only when the image does.
	epoch := img.Created
	if epoch.IsZero() {
		epoch = time.Unix(0, 0)
	}
	return builtImage{id: id, epoch: epoch}, nil
}

// fetch copies the image that digested names from its registry into the store, named by digest, and returns it.
// It copies the image itself, rather than with buildah.Pull, so that a trace shows the blobs it copies.
func (pl *puller) fetch(ctx context.Context, digested reference.Canonical, platform imgspecv1.Platform) (
	*storage.Image, error) {
	src, err := docker.NewReference(digested)
	if err != nil {
		return nil, err
	}
	dest, err := is.Transport.NewStoreReference(pl.store, digested, "")
	if err != nil {
		return nil, err
	}
	// The system's policy decides which images may be pulled, as it would for buildah.
	policy, err := signature.DefaultPolicy(pl.systemContext)
	if err != nil {
		return nil, err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, err
	}
	defer policyContext.Destroy()
	retries, err := pullRetries()
	if err != nil {
		return nil, err
	}
	sys := forPlatform(pl.systemContext, platform)
	err = retry.IfNecessary(ctx, func() error {
		_, err := tracedCopy(ctx, policyContext, dest, src, &imagecopy.Options{SourceCtx: sys, DestinationCtx: sys})
		return err
	}, retries)
	if err != nil {
		return nil, err
	}
	return is.Transport.GetStoreImage(pl.store, dest)
}

// pullRetries returns how to retry a pull after an error that may not happen again, such as a connection reset:
// as containers.conf's retry and retry_delay say, as for buildah and podman, backing off exponentially from a
// second unless retry_delay is set.
func pullRetries() (*retry.Options, error) {
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

// pullError describes err, from reaching the registry for image, with a hint if the registry has no such image or
// won't show it.
func pullError(image string, err error) error {
	if imageUnavailable(err) {
		return fmt.Errorf("image %q: %w (check the name, and log in if the image is private)", image, err)
	}
	return fmt.Errorf("image %q: %w", image, err)
}

// instance returns the digest of the image that the tag ref names for platform: the tag's own, if it names an
// image, or else the one for platform in the index it names. The record keeps the manifest the tag names, so
// that choosing another platform's image later needs no registry either.
func (pl *puller) instance(ctx context.Context, ref reference.NamedTagged, platform imgspecv1.Platform) (
	digest.Digest, error) {
	record := pl.tags[ref.String()]
	var err error
	if record == nil && !pl.again[ref.String()] {
		if record, err = tagRecordOf(pl.cache, ref); err != nil {
			return "", err
		}
	}
	tracing.Current(ctx).Set("tag recorded", record != nil)
	if record == nil {
		data, mimeType, err := pl.manifest(ctx, ref)
		if err != nil {
			return "", err
		}
		d, err := manifest.Digest(data)
		if err != nil {
			return "", err
		}
		record = &tagRecord{Digest: d, MIMEType: mimeType, Manifest: data}
	}
	pl.tags[ref.String()] = record
	tracing.Current(ctx).Set("manifest", record.Digest.String())
	if d, ok := record.Instances[platforms.Format(platform)]; ok {
		return d, nil
	}

	data, err := pl.recordedManifest(ctx, ref, record)
	if err != nil {
		return "", err
	}
	d := record.Digest
	if manifest.MIMETypeIsMultiImage(record.MIMEType) {
		if d, err = chooseInstance(data, record.MIMEType, platform); err != nil {
			return "", err
		}
	}
	if record.Instances == nil {
		record.Instances = map[string]digest.Digest{}
	}
	record.Instances[platforms.Format(platform)] = d
	if err := setTagRecord(pl.cache, ref, record); err != nil {
		return "", fmt.Errorf("saving to the build cache: %w", err)
	}
	return d, nil
}

// recordedManifest returns the manifest that record says the tag ref named: as recorded, or else, by its
// digest, which names what the tag did then, from the registry.
func (pl *puller) recordedManifest(ctx context.Context, ref reference.NamedTagged, record *tagRecord) (
	[]byte, error) {
	if record.Manifest != nil {
		return record.Manifest, nil
	}
	digested, err := reference.WithDigest(reference.TrimNamed(ref), record.Digest)
	if err != nil {
		return nil, err
	}
	data, mimeType, err := pl.manifest(ctx, digested)
	if err != nil {
		return nil, err
	}
	record.MIMEType, record.Manifest = mimeType, data
	return data, nil
}

// manifestTimeout limits fetching a manifest, which is small, so that a registry that stops answering is
// reported rather than waited for.
const manifestTimeout = time.Minute

// manifest fetches the manifest that ref names from its registry.
func (pl *puller) manifest(ctx context.Context, ref reference.Named) (_ []byte, _ string, err error) {
	ctx, span := tracing.Start(ctx, "resolve "+reference.FamiliarString(ref), "registry", reference.Domain(ref))
	defer func() { span.EndWith(err) }()
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()
	imageRef, err := docker.NewReference(ref)
	if err != nil {
		return nil, "", err
	}
	src, err := imageRef.NewImageSource(ctx, pl.systemContext)
	if err != nil {
		return nil, "", err
	}
	defer src.Close()
	data, mimeType, err := src.GetManifest(ctx, nil)
	if err == nil {
		span.Set("media type", mimeType, "digest", digest.FromBytes(data).String())
	}
	return data, mimeType, err
}

// chooseInstance returns the digest of the image for platform in the index data.
func chooseInstance(data []byte, mimeType string, platform imgspecv1.Platform) (digest.Digest, error) {
	list, err := manifest.ListFromBlob(data, mimeType)
	if err != nil {
		return "", err
	}
	matcher := platforms.NewMatcher(platform)
	var found []string
	for _, d := range list.Instances() {
		instance, err := list.Instance(d)
		if err != nil {
			return "", err
		}
		p := instance.ReadOnly.Platform
		if p == nil || p.OS == "unknown" { // such as an attestation
			continue
		}
		if matcher.Match(*p) {
			return d, nil
		}
		found = append(found, platforms.Format(platforms.Normalize(*p)))
	}
	slices.Sort(found)
	return "", fmt.Errorf("its index has no image for %s; it has %s", platforms.Format(platform),
		fileset.QuoteNames(slices.Compact(found)))
}

// forPlatform returns a copy of sys for images of platform, so that buildah expects that platform rather than
// the host's.
func forPlatform(sys *types.SystemContext, platform imgspecv1.Platform) *types.SystemContext {
	c := *sys
	c.OSChoice = platform.OS
	c.ArchitectureChoice = platform.Architecture
	c.VariantChoice = platform.Variant
	return &c
}

// HostPlatform is the platform of the machine vamos runs on, whose programs need no emulator.
var HostPlatform = platforms.Normalize(platforms.DefaultSpec())

// imagePlatform returns the platform that the image with ID id in store says it is for.
func imagePlatform(ctx context.Context, store storage.Store, id string) (imgspecv1.Platform, error) {
	ref, err := is.Transport.NewStoreReference(store, nil, id)
	if err != nil {
		return imgspecv1.Platform{}, err
	}
	img, err := ref.NewImage(ctx, nil)
	if err != nil {
		return imgspecv1.Platform{}, err
	}
	defer img.Close()
	config, err := img.OCIConfig(ctx)
	if err != nil {
		return imgspecv1.Platform{}, err
	}
	return platforms.Normalize(imgspecv1.Platform{
		OS:           config.OS,
		Architecture: config.Architecture,
		Variant:      config.Variant,
	}), nil
}
