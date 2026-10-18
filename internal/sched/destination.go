package sched

import (
	"fmt"

	"go.podman.io/image/v5/docker/reference"
)

// ParseDestination returns the tag that a push destination names: a full registry reference with a tag and
// no digest, such as ghcr.io/me/app:1.0, which the client resolved, so that both sides mean the same registry.
func ParseDestination(destination string) (reference.NamedTagged, error) {
	named, err := reference.ParseNamed(destination)
	if err != nil {
		return nil, fmt.Errorf("push destination %q: %w", destination, err)
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("push destination %q must not include a digest", destination)
	}
	dest, ok := named.(reference.NamedTagged)
	if !ok {
		return nil, fmt.Errorf("push destination %q has no tag", destination)
	}
	return dest, nil
}
