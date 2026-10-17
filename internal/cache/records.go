package cache

import (
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
)

// ActionResult is the latest result of an action.
// Everything it names is in the content store or, for a layer's image, in the executor's image store,
// so a result can be checked before it is reused.
type ActionResult struct {
	Image    digest.Digest `json:"image,omitempty"`    // a layer's or pull's image: its config's digest, its ID
	Platform string        `json:"platform,omitempty"` // the image's, as platforms.Format gives it
	Index    digest.Digest `json:"index,omitempty"`    // a resolve's: the manifest its tag named, an index or an image
	Output   *Blob         `json:"output,omitempty"`   // a task's files, as a tree
	Log      *Blob         `json:"log,omitempty"`      // what its run() steps printed, if anything
	Epoch    int64         `json:"epoch"`              // the time its files were stamped with, in Unix seconds
	// Provenance says what made the result; it is no part of the action's key.
	Provenance *Provenance `json:"provenance,omitempty"`
}

// Provenance describes the executor that made a result.
type Provenance struct {
	Platform     string   `json:"platform,omitempty"`     // the executor's, as platforms.Format gives it
	Isolation    string   `json:"isolation,omitempty"`    // how it ran commands, such as "chroot"
	Capabilities []string `json:"capabilities,omitempty"` // the Linux capabilities it gave run() steps
	Version      string   `json:"version,omitempty"`      // of vamos, if known
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
	return nil
}

// TagRecord is what a pull()'s tag named when it was last resolved, kept so that later builds pull the same images,
// without reaching the registry, until --rebuild resolves it again.
type TagRecord struct {
	Digest    digest.Digest            `json:"digest"`              // of the manifest it named: an index or an image
	MIMEType  string                   `json:"mime_type,omitempty"` // the manifest's
	Manifest  []byte                   `json:"manifest,omitempty"`  // the manifest itself, a few KB
	Instances map[string]digest.Digest `json:"instances,omitempty"` // each platform's image, by platform, once chosen
}

// Validate checks that the record's digests are well formed.
func (r *TagRecord) Validate() error {
	if r == nil {
		return errors.New("no record")
	}
	if err := r.Digest.Validate(); err != nil {
		return fmt.Errorf("digest %q: %w", r.Digest, err)
	}
	for platform, d := range r.Instances {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("instance for %s, %q: %w", platform, d, err)
		}
	}
	return nil
}
