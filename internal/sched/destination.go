package sched

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Topher-the-Geek/vamos/internal/api"

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

// ParsePublication returns the repositories that req publishes to, one for each of its images, in order,
// and for each the tags to write there, each a destination as ParseDestination returns it.
// Every repository must be a full registry name with no tag or digest, which the client resolved,
// and no two the same; every result must have made an image or index;
// there must be an image and a tag, and every tag a valid one, given once, and no more destinations than
// MaxDestinations.
func ParsePublication(req api.PublishRequest) ([]reference.Named, [][]reference.NamedTagged, error) {
	if len(req.Images) == 0 {
		return nil, nil, errors.New("a publication must publish an image")
	}
	if len(req.Tags) == 0 {
		return nil, nil, errors.New("a publication must give a tag")
	}
	if err := CheckDestinations(len(req.Images), len(req.Tags)); err != nil {
		return nil, nil, err
	}
	for i, tag := range req.Tags {
		if slices.Contains(req.Tags[:i], tag) {
			return nil, nil, fmt.Errorf("a publication gives tag %q twice", tag)
		}
	}
	repos := make([]reference.Named, len(req.Images))
	dests := make([][]reference.NamedTagged, len(req.Images))
	for i, image := range req.Images {
		repo, err := ParseRepository(image.Repository)
		if err != nil {
			return nil, nil, err
		}
		for _, other := range repos[:i] {
			if other.Name() == repo.Name() {
				return nil, nil, fmt.Errorf("a publication publishes to repository %q twice", image.Repository)
			}
		}
		if image.Result.Image == "" && image.Result.Index == nil {
			return nil, nil, fmt.Errorf("the result to publish to %s made no image", image.Repository)
		}
		repos[i] = repo
		for _, tag := range req.Tags {
			dest, err := reference.WithTag(repo, tag)
			if err != nil {
				return nil, nil, fmt.Errorf("publication tag %q: %w", tag, err)
			}
			dests[i] = append(dests[i], dest)
		}
	}
	return repos, dests, nil
}

// MaxDestinations is the most destinations, repositories times tags, that a publication writes. A publication's
// outcome names every one, and must fit what a worker reports and an executor returns; the bound also keeps a
// publication's checks from making more than that before it is refused.
const MaxDestinations = 1000

// CheckDestinations refuses a publication of images images, each under tags tags, that writes more than
// MaxDestinations destinations.
func CheckDestinations(images, tags int) error {
	if images > MaxDestinations || tags > MaxDestinations || images*tags > MaxDestinations {
		return fmt.Errorf("a publication of %d images under %d tags writes %d destinations, more than %d; "+
			"publish fewer at once", images, tags, images*tags, MaxDestinations)
	}
	return nil
}

// ParseRepository returns the repository that a publication's repository names: a full registry name with no tag
// or digest, such as ghcr.io/me/app, which the client resolved, so that both sides mean the same registry.
func ParseRepository(repository string) (reference.Named, error) {
	repo, err := reference.ParseNamed(repository)
	if err != nil {
		return nil, fmt.Errorf("publication repository %q: %w", repository, err)
	}
	if !reference.IsNameOnly(repo) {
		return nil, fmt.Errorf("publication repository %q must have no tag or digest", repository)
	}
	return repo, nil
}
