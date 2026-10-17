package run

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

// TestBlobTracker checks the spans made from a copy's events, as containers/image sends them:
// a skipped blob has no start, a blob being copied when the copy fails or is cancelled has no end,
// and the same blob can be copied twice at once.
func TestBlobTracker(t *testing.T) {
	a := types.BlobInfo{Digest: digest.FromString("a"), Size: 10, MediaType: "layer"}
	b := types.BlobInfo{Digest: digest.FromString("b"), Size: -1}
	event := func(e types.ProgressEvent, info types.BlobInfo, offset uint64) types.ProgressProperties {
		return types.ProgressProperties{Event: e, Artifact: info, Offset: offset}
	}
	for _, tt := range []struct {
		name   string
		events []types.ProgressProperties
		cancel bool
		err    error
		want   []string // each span: its name, and its outcome or bytes sent
	}{
		{
			name: "copied and skipped",
			events: []types.ProgressProperties{
				event(types.ProgressEventNewArtifact, a, 0),
				event(types.ProgressEventRead, a, 4),
				event(types.ProgressEventSkipped, b, 0),
				event(types.ProgressEventDone, a, 7),
			},
			want: []string{
				"skipped blob " + shortDigest(b.Digest) + " instant",
				"blob " + shortDigest(a.Digest) + " sent 7",
			},
		},
		{
			name: "failed",
			events: []types.ProgressProperties{
				event(types.ProgressEventNewArtifact, a, 0),
				event(types.ProgressEventNewArtifact, b, 0),
				event(types.ProgressEventDone, b, 3),
			},
			err:  errors.New("connection reset"),
			want: []string{"blob " + shortDigest(b.Digest) + " sent 3", "blob " + shortDigest(a.Digest) + " failed"},
		},
		{
			name: "cancelled",
			events: []types.ProgressProperties{
				event(types.ProgressEventNewArtifact, a, 0),
			},
			cancel: true,
			err:    context.Canceled,
			want:   []string{"blob " + shortDigest(a.Digest) + " cancelled"},
		},
		{
			name: "the same blob twice",
			events: []types.ProgressProperties{
				event(types.ProgressEventNewArtifact, a, 0),
				event(types.ProgressEventNewArtifact, a, 0),
				event(types.ProgressEventDone, a, 1),
				event(types.ProgressEventDone, a, 2),
			},
			want: []string{"blob " + shortDigest(a.Digest) + " sent 1", "blob " + shortDigest(a.Digest) + " sent 2"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := tracing.NewRecorder("executor", nil)
			ctx, cancel := context.WithCancel(tracing.WithRecorder(context.Background(), r, ""))
			defer cancel()
			blobs := newBlobTracker(ctx)
			for _, e := range tt.events {
				blobs.event(e)
			}
			if tt.cancel {
				cancel()
			}
			blobs.close(ctx, tt.err)

			var got []string
			for _, s := range r.Spans() {
				switch {
				case s.Instant:
					got = append(got, s.Name+" instant")
					if s.Args["already present"] != true || s.Args["source digest"] != b.Digest.String() {
						t.Errorf("skipped blob's args = %v", s.Args)
					}
					if _, ok := s.Args["source size"]; ok {
						t.Error("a blob of unknown size has a size")
					}
				case s.Args["outcome"] != nil:
					got = append(got, s.Name+" "+s.Args["outcome"].(string))
					if s.Args["error"] != tt.err.Error() {
						t.Errorf("unfinished blob's error = %v, want %v", s.Args["error"], tt.err)
					}
				default:
					got = append(got, s.Name+" sent "+formatUint(s.Args["bytes sent"]))
				}
				if !s.Instant && !s.Async {
					t.Errorf("blob %q isn't async", s.Name)
				}
			}
			if len(got) != len(tt.want) {
				t.Fatalf("spans = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("spans = %q, want %q", got, tt.want)
				}
			}
			if len(blobs.open) != 0 {
				t.Errorf("%d blobs still open", len(blobs.open))
			}
		})
	}
}

// formatUint formats v, a uint64 argument, for comparison.
func formatUint(v any) string {
	n, ok := v.(uint64)
	if !ok {
		return fmt.Sprintf("%v (%T)", v, v)
	}
	return strconv.FormatUint(n, 10)
}
