package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/sched"
)

// ImageChecker checks that an image is where a result's location says it is.
type ImageChecker interface {
	// CheckImage reports why the image with ID id isn't at the location, if it isn't: gone, or another image;
	// a registry that can't be reached is an error.
	CheckImage(ctx context.Context, id string, location cache.Location) (problem, err error)
	// ClearRepository removes every tag of the registry repository named, as a prune does to the cache repository,
	// returning how many there were; a repository the registry doesn't have has none.
	ClearRepository(ctx context.Context, repository string) (int, error)
}

// checker checks cached results against the coordinator's content store and the registry:
// a task's files must be in the content store, and a layer's image at one of its registry locations.
// A resolve needs no check: the pull-through cache serves a base by digest on demand.
type checker struct {
	content cache.ContentStore
	images  ImageChecker
}

// Check reports why result, the action a's, can't be used, if it can't.
func (c *checker) Check(ctx context.Context, a *sched.Action, result *cache.ActionResult) (problem, err error) {
	switch a.Kind {
	case sched.KindTask:
		if a.Output == nil {
			return nil, nil
		}
		if result.Output == nil {
			return errors.New("it has no files"), nil
		}
		if err := cache.CheckTree(ctx, c.content, *result.Output); err != nil {
			return sched.StoreProblem("its files can't be used from the cache", err)
		}
	case sched.KindLayer:
		if result.Image == "" {
			return errors.New("it has no image"), nil
		}
		if result.Platform == "" {
			return errors.New("it doesn't record its image's platform"), nil
		}
		var found bool
		var problems []error
		for _, l := range result.Locations {
			if l.Kind != cache.LocationRegistry {
				continue
			}
			found = true
			if c.images == nil {
				return nil, nil
			}
			problem, err := c.images.CheckImage(ctx, result.Image.Encoded(), l)
			if err != nil {
				return nil, err
			}
			if problem == nil {
				return nil, nil
			}
			problems = append(problems, fmt.Errorf("at %s: %w", l.Reference, problem))
		}
		if !found {
			return errors.New("its image is in no registry"), nil
		}
		return fmt.Errorf("its image %s isn't where it was: %w", result.Image.Encoded()[:12],
			errors.Join(problems...)), nil
	case sched.KindResolve:
		if result.Image == "" || result.Platform == "" {
			return errors.New("it records no image"), nil
		}
	}
	return nil, nil
}
