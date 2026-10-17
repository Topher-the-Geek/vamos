package run

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/transfer"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
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

// tagRecord is what a pull()'s tag named when vamos last resolved it, kept in the record store
// so that later builds pull the same images, without reaching the registry, until --rebuild resolves it again.
type tagRecord = cache.TagRecord

// puller executes resolve actions: it resolves a tag to the image it names for a platform, pulls that image
// into the store unless it is there, and leaves a tag record saying what the tag named.
type puller struct {
	store         storage.Store
	records       cache.RecordStore
	systemContext *types.SystemContext
}

// resolve resolves req, a resolve action, as a span of ctx's trace, and returns the image's ID, platform and epoch,
// and the index, or image, that its tag named.
func (pl *puller) resolve(ctx context.Context, req actionRequest) (*cache.ActionResult, error) {
	a := req.action
	image := a.Image
	named, err := reference.ParseNamed(image)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	tagged, ok := named.(reference.NamedTagged)
	if !ok {
		return nil, fmt.Errorf("image %q has no tag", image)
	}
	platform, err := graph.ParsePlatform(a.Platform)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	instance, record, err := pl.instance(ctx, tagged, platform, a.Index, req.again)
	if err != nil {
		return nil, pullError(image, err)
	}
	digested, err := reference.WithDigest(reference.TrimNamed(tagged), instance)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	tracing.Current(ctx).Set("instance", instance.String())
	// Look for the image pulled before, by its manifest's digest,
	// since copying it asks the registry about an image even if the store has it.
	// A pulled image is named by digest, since a tag can name only one platform's image in the store,
	// but one pulled before by its tag may have only that name.
	// Keep this lookup: redundant copies can exhaust Docker Hub's anonymous pull allowance,
	// making cached builds fail or spend time retrying registry requests.
	// TestPullTag counts requests and requires none once the tag record and images are cached.
	// Current limits: https://docs.docker.com/docker-hub/usage/pulls/
	var img *storage.Image
	images, err := pl.store.ImagesByDigest(instance)
	if err == nil && len(images) > 0 {
		img = images[0]
		tracing.Current(ctx).Set("already present", true)
	} else if err == nil || errors.Is(err, storage.ErrImageUnknown) {
		img, err = pl.fetch(ctx, digested, platform)
		if err != nil {
			// With the tag's record cached, this is the first request to reach the registry.
			return nil, pullError(image, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	// What the tag resolved to is validated before it is used: the store's image must be the manifest chosen,
	// its ID is its config's digest, and it must be for the platform asked.
	// containers/image checks the manifest against the digest it fetched too; this guards the store's index.
	if img.Digest != instance && !slices.Contains(img.Digests, instance) {
		return nil, fmt.Errorf("image %q: the store's image for manifest %s isn't that manifest", image, instance)
	}
	id, got, err := imageIdentity(ctx, pl.store, img.ID)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	if !platforms.NewMatcher(platform).Match(got) {
		return nil, fmt.Errorf("image %q is for %s, not %s, and its tag names that image alone, "+
			"not an index of images for several platforms", image, platforms.Format(got), platforms.Format(platform))
	}
	// The image is located by its ID through the cache repository from now on, as a built layer is.
	if err := nameImage(pl.store, img, id.Encoded()); err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	// The record is written once the image is in the store, so that no record names an image never pulled.
	if err := pl.records.SetTag(ctx, tagged.String(), record); err != nil {
		return nil, fmt.Errorf("saving to the build cache: %w", err)
	}
	// Files built on the image are stamped with its creation time:
	// a time that is the same on every machine, and that moves forward only when the image does.
	epoch := img.Created
	if epoch.IsZero() {
		epoch = time.Unix(0, 0)
	}
	return &cache.ActionResult{
		Image:    id,
		Platform: platforms.Format(got),
		Index:    record.Digest,
		Epoch:    epoch.Unix(),
	}, nil
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
	retries, err := transfer.Retries()
	if err != nil {
		return nil, err
	}
	sys := forPlatform(pl.systemContext, platform)
	err = retry.IfNecessary(ctx, func() error {
		_, err := transfer.Image(ctx, policyContext, dest, src, &imagecopy.Options{SourceCtx: sys, DestinationCtx: sys})
		return err
	}, retries)
	if err != nil {
		return nil, err
	}
	return is.Transport.GetStoreImage(pl.store, dest)
}

// pullError describes err, from reaching the registry for image,
// with a hint if the registry has no such image or won't show it.
func pullError(image string, err error) error {
	if imageUnavailable(err) {
		return fmt.Errorf("image %q: %w (check the name, and log in if the image is private)", image, err)
	}
	return fmt.Errorf("image %q: %w", image, err)
}

// instance returns the digest of the image that the tag ref names for platform, and the tag's record:
// the tag's own manifest, if it names an image, or else the one for platform in the index it names.
// With pinned, the tag is taken to name that manifest, as it did earlier in the job;
// otherwise the tag is resolved as the record store has it, or afresh if again or it has no record.
// The record keeps the manifest the tag names, so that choosing another platform's image later needs no registry;
// the caller saves it once the image is in the store.
func (pl *puller) instance(
	ctx context.Context, ref reference.NamedTagged, platform imgspecv1.Platform, pinned digest.Digest, again bool,
) (digest.Digest, *tagRecord, error) {
	var record *tagRecord
	var err error
	if pinned != "" || !again {
		if record, err = pl.records.Tag(ctx, ref.String()); err != nil {
			return "", nil, err
		}
	}
	tracing.Current(ctx).Set("tag recorded", record != nil)
	if pinned != "" && (record == nil || record.Digest != pinned) {
		record = &tagRecord{Digest: pinned} // the manifest is fetched by its digest, below
	}
	if record == nil {
		data, mimeType, err := pl.manifest(ctx, ref)
		if err != nil {
			return "", nil, err
		}
		d, err := manifest.Digest(data)
		if err != nil {
			return "", nil, err
		}
		record = &tagRecord{Digest: d, MIMEType: mimeType, Manifest: data}
	}
	tracing.Current(ctx).Set("manifest", record.Digest.String())
	if d, ok := record.Instances[platforms.Format(platform)]; ok {
		return d, record, nil
	}

	data, err := pl.recordedManifest(ctx, ref, record)
	if err != nil {
		return "", nil, err
	}
	d := record.Digest
	if manifest.MIMETypeIsMultiImage(record.MIMEType) {
		if d, err = chooseInstance(data, record.MIMEType, platform); err != nil {
			return "", nil, err
		}
	}
	if record.Instances == nil {
		record.Instances = map[string]digest.Digest{}
	}
	record.Instances[platforms.Format(platform)] = d
	return d, record, nil
}

// recordedManifest returns the manifest that record says the tag ref named: as recorded,
// or else, by its digest, which names what the tag did then, from the registry.
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
	d, err := manifest.Digest(data)
	if err != nil {
		return nil, err
	}
	if d != record.Digest {
		return nil, fmt.Errorf("the registry gave manifest %s for %s", d, digested.Digest())
	}
	record.MIMEType, record.Manifest = mimeType, data
	return data, nil
}

// manifestTimeout limits fetching a manifest, which is small,
// so that a registry that stops answering is reported rather than waited for.
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

// forPlatform returns a copy of sys for images of platform,
// so that buildah expects that platform rather than the host's.
func forPlatform(sys *types.SystemContext, platform imgspecv1.Platform) *types.SystemContext {
	c := *sys
	c.OSChoice = platform.OS
	c.ArchitectureChoice = platform.Architecture
	c.VariantChoice = platform.Variant
	return &c
}

// HostPlatform is the platform of the machine vamos runs on, whose programs need no emulator.
var HostPlatform = platforms.Normalize(platforms.DefaultSpec())

// imageIdentity returns the image ID of the image that storeID names in store, the digest of its config,
// and the platform its config says it is for.
// The store's own IDs stay within run; results and names use the image ID.
// An image with no config, as the retired Docker schema 1 makes, has no image ID and can't be used,
// and nor can one whose config's digest isn't SHA-256, the algorithm image IDs are named in.
// The image's layers in the store must be those its config lists, so that its ID identifies its files too.
func imageIdentity(ctx context.Context, store storage.Store, storeID string) (
	digest.Digest, imgspecv1.Platform, error) {
	ref, err := is.Transport.NewStoreReference(store, nil, storeID)
	if err != nil {
		return "", imgspecv1.Platform{}, err
	}
	img, err := ref.NewImage(ctx, nil)
	if err != nil {
		return "", imgspecv1.Platform{}, err
	}
	defer img.Close()
	configDigest := img.ConfigInfo().Digest
	if configDigest == "" {
		return "", imgspecv1.Platform{}, errors.New("the image has no config, so no image ID")
	}
	if err := configDigest.Validate(); err != nil {
		return "", imgspecv1.Platform{}, fmt.Errorf("the image's config digest %q: %w", configDigest, err)
	}
	if configDigest.Algorithm() != digest.SHA256 {
		return "", imgspecv1.Platform{}, fmt.Errorf("the image's config digest %s isn't SHA-256", configDigest)
	}
	config, err := img.OCIConfig(ctx)
	if err != nil {
		return "", imgspecv1.Platform{}, err
	}
	stored, err := store.Image(storeID)
	if err != nil {
		return "", imgspecv1.Platform{}, err
	}
	if err := checkLayers(store, stored, config.RootFS.DiffIDs); err != nil {
		return "", imgspecv1.Platform{}, err
	}
	return configDigest, platforms.Normalize(imgspecv1.Platform{
		OS:           config.OS,
		Architecture: config.Architecture,
		Variant:      config.Variant,
	}), nil
}

// checkLayers checks that img's layers in store are, from the bottom, those with the uncompressed digests diffIDs.
// containers/image checks a pulled layer against its config's diff ID unless storage.conf's
// insecure_allow_unpredictable_image_contents lets a partial pull keep a layer that only its table of contents
// identifies, with no uncompressed digest: an image whose config wouldn't identify its files.
func checkLayers(store storage.Store, img *storage.Image, diffIDs []digest.Digest) error {
	var stored []digest.Digest
	// A layer chain longer than the config's can't match, which bounds a walk of a corrupt one.
	for id := img.TopLayer; id != "" && len(stored) <= len(diffIDs); {
		layer, err := store.Layer(id)
		if err != nil {
			return err
		}
		stored = append(stored, layer.UncompressedDigest)
		id = layer.Parent
	}
	slices.Reverse(stored)
	if !slices.Equal(stored, diffIDs) {
		return errors.New("the image's layers in the container store aren't those its config lists, " +
			"as when storage.conf's insecure_allow_unpredictable_image_contents lets a partial pull skip checking them")
	}
	return nil
}

// errWrongImage is the error that the image a name in the cache repository names isn't the image the name is for.
var errWrongImage = errors.New("the image named for it isn't that image")

// locateImage returns the container store's own ID of the image with image ID id, and the platform it is for.
// The image is found by its name in the cache repository, which anything changing the store can move to another,
// so what the name names is checked to be the image, and then used by the store's ID found, which can't move.
// It returns an error wrapping storage.ErrImageUnknown if nothing has the name,
// and one wrapping errWrongImage if what does isn't the image, or can't be read as an image.
func locateImage(ctx context.Context, store storage.Store, id string) (string, imgspecv1.Platform, error) {
	img, err := store.Image(cacheName(id))
	if err != nil {
		return "", imgspecv1.Platform{}, err
	}
	got, platform, err := imageIdentity(ctx, store, img.ID)
	if err != nil {
		return "", imgspecv1.Platform{}, fmt.Errorf("%w: %w", errWrongImage, err)
	}
	if got.Encoded() != id {
		return "", imgspecv1.Platform{}, fmt.Errorf("%w: it is image %s", errWrongImage, got.Encoded()[:12])
	}
	return img.ID, platform, nil
}

// cacheName returns the name the image with ID id has in the cache repository, through which run locates it.
func cacheName(id string) string {
	return CacheRepository + ":" + id
}

// nameImage gives img, in store, its name in the cache repository, by its image ID id, unless it has it.
func nameImage(store storage.Store, img *storage.Image, id string) error {
	name := cacheName(id)
	if slices.Contains(img.Names, name) {
		return nil
	}
	if err := store.AddNames(img.ID, []string{name}); err != nil {
		return fmt.Errorf("naming image %s: %w", shortID(id), err)
	}
	return nil
}
