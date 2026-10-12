package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
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

// tagPath returns where the record of the tag ref is kept.
func (c *cache) tagPath(ref reference.NamedTagged) string {
	return filepath.Join(c.dir, "tags", digest.FromString(ref.String()).Encoded())
}

// tag returns the record of the tag ref, or nil if there is none.
func (c *cache) tag(ref reference.NamedTagged) (*tagRecord, error) {
	data, err := os.ReadFile(c.tagPath(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var record tagRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Digest == "" {
		return nil, nil // as good as none: the tag is resolved again
	}
	return &record, nil
}

// setTag records what the tag ref names, replacing any earlier record.
func (c *cache) setTag(ref reference.NamedTagged, record *tagRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := c.create()
	if err != nil {
		return err
	}
	defer f.discard()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.commit(c.tagPath(ref))
}

// puller pulls the images of a build's pulls, each tag resolved once in the build, so that one tag gives every
// platform's image from the same index, even if the tag moves meanwhile.
type puller struct {
	store         storage.Store
	cache         *cache
	systemContext *types.SystemContext
	tags          map[string]*tagRecord // resolved in this build, by tag
	again         map[string]bool       // tags to resolve again, rather than as the build cache records them
}

// pull fetches the image of p, for its configuration's platform, into the store, unless it is there already.
func (pl *puller) pull(ctx context.Context, p *pullSpec, platform *platformValue) (builtImage, error) {
	image := p.image.String()
	instance, err := pl.instance(ctx, p.image, platform)
	if err != nil {
		return builtImage{}, pullError(image, err)
	}
	digested, err := reference.WithDigest(reference.TrimNamed(p.image), instance)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	// Look for the image pulled before, by its manifest's digest, since buildah.Pull asks the registry about an
	// image even if the store has it. A pulled image is named by digest, since a tag can name only one
	// platform's image in the store, but one pulled before by its tag may have only that name.
	var img *storage.Image
	images, err := pl.store.ImagesByDigest(instance)
	if err == nil && len(images) > 0 {
		img = images[0]
	} else if err == nil || errors.Is(err, storage.ErrImageUnknown) {
		var id string
		id, err = buildah.Pull(ctx, digested.String(), buildah.PullOptions{
			Store:         pl.store,
			SystemContext: forPlatform(pl.systemContext, platform),
			PullPolicy:    buildah.PullIfMissing,
		})
		if err != nil {
			// With the tag's record cached, this is the first request to reach the registry.
			return builtImage{}, pullError(image, err)
		}
		// buildah.Pull can't be cancelled, so check for an interrupt after it.
		if err := ctx.Err(); err != nil {
			return builtImage{}, err
		}
		img, err = pl.store.Image(id)
	}
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	id := img.ID
	got, err := imagePlatform(ctx, pl.store, id)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	if !platforms.NewMatcher(platform.spec).Match(got) {
		return builtImage{}, fmt.Errorf("image %q is for %s, not %s, and its tag names that image alone, "+
			"not an index of images for several platforms", image, platforms.Format(got), platform)
	}
	// Files built on the image are stamped with its creation time: a time that is the same on
	// every machine, and that moves forward only when the image does.
	epoch := img.Created
	if epoch.IsZero() {
		epoch = time.Unix(0, 0)
	}
	return builtImage{id: id, epoch: epoch}, nil
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
func (pl *puller) instance(ctx context.Context, ref reference.NamedTagged, platform *platformValue) (
	digest.Digest, error) {
	record := pl.tags[ref.String()]
	var err error
	if record == nil && !pl.again[ref.String()] {
		if record, err = pl.cache.tag(ref); err != nil {
			return "", err
		}
	}
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
	if d, ok := record.Instances[platform.String()]; ok {
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
	record.Instances[platform.String()] = d
	if err := pl.cache.setTag(ref, record); err != nil {
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
func (pl *puller) manifest(ctx context.Context, ref reference.Named) ([]byte, string, error) {
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
	return src.GetManifest(ctx, nil)
}

// chooseInstance returns the digest of the image for platform in the index data.
func chooseInstance(data []byte, mimeType string, platform *platformValue) (digest.Digest, error) {
	list, err := manifest.ListFromBlob(data, mimeType)
	if err != nil {
		return "", err
	}
	matcher := platforms.NewMatcher(platform.spec)
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
	return "", fmt.Errorf("its index has no image for %s; it has %s", platform, quoteNames(slices.Compact(found)))
}

// forPlatform returns a copy of sys for images of platform, so that buildah expects that platform rather than
// the host's.
func forPlatform(sys *types.SystemContext, platform *platformValue) *types.SystemContext {
	c := *sys
	c.OSChoice = platform.spec.OS
	c.ArchitectureChoice = platform.spec.Architecture
	c.VariantChoice = platform.spec.Variant
	return &c
}

// hostPlatform is the platform of the machine vamos runs on, whose programs need no emulator.
var hostPlatform = platforms.Normalize(platforms.DefaultSpec())

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
