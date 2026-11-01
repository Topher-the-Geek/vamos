package sched

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
)

// TestParsePublicationBoundsDestinations checks that a publication may write MaxDestinations destinations, but is
// refused, saying why, if its repositories times its tags would write more.
func TestParsePublicationBoundsDestinations(t *testing.T) {
	publication := func(images, tags int) api.PublishRequest {
		var req api.PublishRequest
		for i := range images {
			req.Images = append(req.Images, api.PublishImage{
				Result:     api.Result{Image: strings.Repeat("a", 64)},
				Repository: fmt.Sprintf("example.com/team/app%d", i),
			})
		}
		for i := range tags {
			req.Tags = append(req.Tags, fmt.Sprintf("v%d", i))
		}
		return req
	}
	if _, dests, err := ParsePublication(publication(10, MaxDestinations/10)); err != nil || len(dests) != 10 {
		t.Errorf("a publication of %d destinations: %v, want it parsed", MaxDestinations, err)
	}
	for _, size := range [][2]int{{10, MaxDestinations/10 + 1}, {1, MaxDestinations + 1}, {MaxDestinations + 1, 1}} {
		_, _, err := ParsePublication(publication(size[0], size[1]))
		if err == nil || !strings.Contains(err.Error(), "publish fewer at once") {
			t.Errorf("a publication of %d images under %d tags: %v, want it refused as too many destinations",
				size[0], size[1], err)
		}
	}
}
