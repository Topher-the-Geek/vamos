package cache

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
)

// ActionResult is the latest result of an action.
// Everything it names is in the content store or, for an image, at one of its locations,
// so a result can be checked before it is reused.
type ActionResult struct {
	Image    digest.Digest `json:"image,omitempty"`    // a layer's or pull's image: its config's digest, its ID
	Platform string        `json:"platform,omitempty"` // the image's, as platforms.Format gives it
	Index    digest.Digest `json:"index,omitempty"`    // a resolve's: the manifest its tag named, an index or an image
	// Locations is where the image's bytes are, in the order to try them; the image is identified by Image alone.
	Locations []Location `json:"locations,omitempty"`
	Output    *Blob      `json:"output,omitempty"` // a task's files, as a tree
	Log       *Blob      `json:"log,omitempty"`    // what its run() steps printed, if anything
	Epoch     int64      `json:"epoch"`            // the time its files were stamped with, in Unix seconds
	// Provenance says what made the result; it is no part of the action's key.
	Provenance *Provenance `json:"provenance,omitempty"`
}

// Provenance describes the executor that made a result.
type Provenance struct {
	Platform     string   `json:"platform,omitempty"`     // the executor's, as platforms.Format gives it
	Isolation    string   `json:"isolation,omitempty"`    // how it ran commands, such as "chroot"
	Capabilities []string `json:"capabilities,omitempty"` // the Linux capabilities it gave run() steps
	Version      string   `json:"version,omitempty"`      // of vamos, if known
	Emulated     bool     `json:"emulated,omitempty"`     // its run() steps ran under emulation, for another platform
}

// Location is a place an image's bytes can be fetched from.
// Kind says what Reference names; an image is identified by its ID wherever it is, never by a location.
type Location struct {
	Kind      string `json:"kind"`      // LocationStore or LocationRegistry
	Reference string `json:"reference"` // an image name in the store, or a registry reference with a tag or digest
}

// The kinds of location.
const (
	LocationStore    = "store"    // the executor's own container store, where Reference is the image's name
	LocationRegistry = "registry" // an OCI registry, where Reference is a repository with a tag or a digest
)

// Validate checks that l names a place as a known kind.
func (l Location) Validate() error {
	switch l.Kind {
	case LocationStore, LocationRegistry:
	default:
		return fmt.Errorf("location kind %q isn't known", l.Kind)
	}
	if l.Reference == "" {
		return fmt.Errorf("%s location without a reference", l.Kind)
	}
	return nil
}

// Validate checks that what r names is well formed, so that a corrupt record can't name a path.
func (r *ActionResult) Validate() error {
	if r == nil {
		return errors.New("no result")
	}
	for name, d := range map[string]digest.Digest{"image": r.Image, "index": r.Index} {
		if d != "" {
			if err := d.Validate(); err != nil {
				return fmt.Errorf("%s %q: %w", name, d, err)
			}
		}
	}
	for name, b := range map[string]*Blob{"output": r.Output, "log": r.Log} {
		if b != nil {
			if err := b.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	for _, l := range r.Locations {
		if err := l.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// TagRecord is what a pull()'s tag named when it was last resolved, kept so that later builds pull the same images,
// without reaching the registry, until --rebuild resolves it again.
type TagRecord struct {
	Digest   digest.Digest `json:"digest"`              // of the manifest it named: an index or an image
	MIMEType string        `json:"mime_type,omitempty"` // the manifest's
	Manifest []byte        `json:"manifest,omitempty"`  // the manifest itself, a few KB
	// Instances is each platform's image, by platform, once chosen, and what was found out about it once pulled.
	Instances map[string]TagInstance `json:"instances,omitempty"`
}

// TagInstance is the image that a tag named for one platform: the manifest chosen for it from the index the tag
// named, or the tag's own, and, once the image was pulled, what a build needs to use it without resolving the tag
// again: its ID, platform, epoch and locations.
// An instance with only its manifest's digest is incomplete, as records written before the rest was kept are,
// and a resolve completes it.
type TagInstance struct {
	Digest   digest.Digest `json:"digest"`             // of the image's manifest
	Image    digest.Digest `json:"image,omitempty"`    // its ID: its config's digest
	Platform string        `json:"platform,omitempty"` // the image's own, as platforms.Format gives it
	// Epoch is when the image was made, in Unix seconds, which files built on it are stamped with;
	// an image without a creation time has epoch 0.
	Epoch     int64      `json:"epoch,omitempty"`
	Locations []Location `json:"locations,omitempty"` // where its bytes are
}

// Complete reports whether the instance says everything a build needs to use the image without resolving its tag.
func (i TagInstance) Complete() bool {
	return i.Digest != "" && i.Image != "" && i.Platform != ""
}

// UnmarshalJSON decodes an instance, or a bare manifest digest, as records written before kept it,
// which makes an incomplete instance.
func (i *TagInstance) UnmarshalJSON(data []byte) error {
	var d digest.Digest
	if err := json.Unmarshal(data, &d); err == nil {
		*i = TagInstance{Digest: d}
		return nil
	}
	type instance TagInstance // without this method, so that the usual decoding applies
	var decoded instance
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = TagInstance(decoded)
	return nil
}

// Validate checks that the record's digests and locations are well formed.
func (r *TagRecord) Validate() error {
	if r == nil {
		return errors.New("no record")
	}
	if err := r.Digest.Validate(); err != nil {
		return fmt.Errorf("digest %q: %w", r.Digest, err)
	}
	for platform, i := range r.Instances {
		if err := i.Digest.Validate(); err != nil {
			return fmt.Errorf("instance for %s, %q: %w", platform, i.Digest, err)
		}
		if i.Image != "" {
			if err := i.Image.Validate(); err != nil {
				return fmt.Errorf("instance for %s, image %q: %w", platform, i.Image, err)
			}
		}
		for _, l := range i.Locations {
			if err := l.Validate(); err != nil {
				return fmt.Errorf("instance for %s: %w", platform, err)
			}
		}
	}
	return nil
}
