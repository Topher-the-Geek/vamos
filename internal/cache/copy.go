package cache

import (
	"context"
	"fmt"
)

// Copy copies the blobs that to lacks, of blobs, from one content store to another, verifying each on the way:
// what a client does to fetch an executor's export, or an executor to fetch a client's sources.
// It returns how many blobs it copied.
func Copy(ctx context.Context, from, to ContentStore, blobs []Blob) (int, error) {
	missing, err := to.Missing(ctx, blobs)
	if err != nil {
		return 0, err
	}
	for i, b := range missing {
		if err := copyOne(ctx, from, to, b); err != nil {
			return i, fmt.Errorf("copying blob %s: %w", b.Digest, err)
		}
	}
	return len(missing), nil
}

// copyOne copies blob b from one store to another.
func copyOne(ctx context.Context, from, to ContentStore, b Blob) error {
	r, err := from.Get(ctx, b)
	if err != nil {
		return err
	}
	defer r.Close()
	return to.Import(ctx, b, r)
}
