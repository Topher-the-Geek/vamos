package transfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestLayout checks that a layout written from a content store holds every blob under its digest, verified,
// names the manifest in its index.json, and lists its blobs back, and that a missing or corrupt blob fails it.
func TestLayout(t *testing.T) {
	ctx := t.Context()
	store := cache.NewMemory()
	config, err := store.PutBytes(ctx, []byte(`{"os":"linux","architecture":"amd64"}`))
	testenv.Must(t, err)
	layer, err := store.PutBytes(ctx, []byte("layer bytes"))
	testenv.Must(t, err)
	m := imgspecv1.Manifest{MediaType: imgspecv1.MediaTypeImageManifest}
	m.SchemaVersion = 2
	m.Config = imgspecv1.Descriptor{MediaType: imgspecv1.MediaTypeImageConfig, Digest: config.Digest,
		Size: config.Size}
	m.Layers = []imgspecv1.Descriptor{{MediaType: imgspecv1.MediaTypeImageLayer, Digest: layer.Digest,
		Size: layer.Size}}
	data, err := json.Marshal(m)
	testenv.Must(t, err)
	manifest, err := store.PutBytes(ctx, data)
	testenv.Must(t, err)

	dir := t.TempDir()
	ref, err := Layout(ctx, store, dir, manifest, imgspecv1.MediaTypeImageManifest, []cache.Blob{config, layer})
	testenv.Must(t, err)
	if ref.Transport().Name() != "oci" {
		t.Errorf("Layout returned a %s reference, want oci", ref.Transport().Name())
	}
	for _, b := range []cache.Blob{manifest, config, layer} {
		got, err := os.ReadFile(LayoutBlobPath(dir, b))
		if err != nil || digest.FromBytes(got) != b.Digest {
			t.Errorf("blob %s in the layout = %q, %v", b.Digest, got, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.json"))
	testenv.Must(t, err)
	var top imgspecv1.Index
	testenv.Must(t, json.Unmarshal(index, &top))
	if len(top.Manifests) != 1 || top.Manifests[0].Digest != manifest.Digest ||
		top.Manifests[0].Annotations[imgspecv1.AnnotationRefName] != "image" {
		t.Errorf("index.json lists %+v, want the manifest as image", top.Manifests)
	}
	blobs, err := LayoutBlobs(dir)
	if err != nil || len(blobs) != 3 {
		t.Errorf("LayoutBlobs = %v, %v; want the three blobs", blobs, err)
	}

	store.Replace(layer, []byte("faux layer!"))
	if _, err := Layout(ctx, store, t.TempDir(), manifest, imgspecv1.MediaTypeImageManifest,
		[]cache.Blob{config, layer}); !errors.Is(err, cache.ErrCorrupt) {
		t.Errorf("Layout with a corrupt blob = %v, want %v", err, cache.ErrCorrupt)
	}
	gone := cache.Blob{Digest: digest.FromString("gone"), Size: 4}
	if _, err := Layout(ctx, store, t.TempDir(), manifest, imgspecv1.MediaTypeImageManifest,
		[]cache.Blob{config, gone}); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Layout with a missing blob = %v, want %v", err, cache.ErrMissing)
	}
}
