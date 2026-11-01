package load

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
)

// aRelease declares two layers on scratch, two tasks that check the first, and the publication "release",
// which checks both tasks and publishes the first layer to two repositories and the second to one,
// choosing the first repository's namespace with its parameter env.
const aRelease = `
layer(name="app", base="scratch")
layer(name="worker", base="scratch")
task(name="lint", base="app")
task(name="test", base="app")
publication(
    name="release",
    params={"env": "prod"},
    checks=["lint", "test"],
    images=lambda ctx: {
        "registry.example/" + ctx.env + "/app": "app",
        "registry.example/mirror/app": "app",
        "registry.example/team/worker": "worker",
    },
)
`

// loadRelease loads targets of aRelease, as want says, with sets, returning the graph, and the publication if wanted.
func loadRelease(t *testing.T, want Want, sets []string, targets ...string) (*graph.Graph, *Publication, error) {
	t.Helper()
	fsys := afero.NewMemMapFs()
	testenv.Must(t, afero.WriteFile(fsys, "/project/BUILD", []byte(aRelease), 0o644))
	req := Request{
		Dir:          "/project",
		Targets:      targets,
		Want:         want,
		Sets:         sets,
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
		Store: func(context.Context, afero.Fs, string, *fileset.Selection) (cache.Blob, fileset.Stats, error) {
			t.Fatal("a glob was read, though the project has none")
			return cache.Blob{}, fileset.Stats{}, nil
		},
	}
	if want == WantPublication {
		return LoadPublication(t.Context(), fsys, req)
	}
	g, err := Load(t.Context(), fsys, req)
	return g, nil, err
}

// rootLabels returns the labels of g's roots, in order.
func rootLabels(g *graph.Graph) []string {
	var labels []string
	for _, r := range g.Roots {
		labels = append(labels, g.Node(r).Label)
	}
	return labels
}

// TestLoadPublication checks that a publication loads as its checks, then its images, each once, as roots,
// and that its plan names each image's root and repository, as its parameters choose them.
func TestLoadPublication(t *testing.T) {
	for _, tc := range []struct {
		name string
		sets []string
		want []PublishedImage
	}{
		{
			name: "defaults",
			want: []PublishedImage{
				{Root: 2, Repository: "registry.example/prod/app"},
				{Root: 2, Repository: "registry.example/mirror/app"},
				{Root: 3, Repository: "registry.example/team/worker"},
			},
		},
		{
			name: "a parameter chooses a repository",
			sets: []string{"env=staging"},
			want: []PublishedImage{
				{Root: 2, Repository: "registry.example/staging/app"},
				{Root: 2, Repository: "registry.example/mirror/app"},
				{Root: 3, Repository: "registry.example/team/worker"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, pub, err := loadRelease(t, WantPublication, tc.sets, "release")
			testenv.Must(t, err)
			if got, want := rootLabels(g), []string{"lint", "test", "app", "worker"}; !slices.Equal(got, want) {
				t.Errorf("roots = %v, want %v", got, want)
			}
			if !slices.Equal(pub.Images, tc.want) {
				t.Errorf("images = %v, want %v", pub.Images, tc.want)
			}
		})
	}
}

// TestBuildPublication checks that building a publication builds its checks and images, as publishing it would.
func TestBuildPublication(t *testing.T) {
	g, _, err := loadRelease(t, WantBuild, nil, "release", "app")
	testenv.Must(t, err)
	if got, want := rootLabels(g), []string{"lint", "test", "app", "worker"}; !slices.Equal(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}

// TestPublicationRefused checks that publish refuses a target that isn't a publication, saying what it is,
// and that the commands sending one image or a task's files refuse a publication, naming those that take it.
func TestPublicationRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   Want
		target string
		error  string
	}{
		{
			name:   "publish a layer",
			want:   WantPublication,
			target: "app",
			error:  `"app" is a layer(), not a publication(); publish takes a publication`,
		},
		{
			name:   "publish a task",
			want:   WantPublication,
			target: "lint",
			error:  `"lint" is a task(), not a publication(); publish takes a publication`,
		},
		{
			name:   "send a publication's image",
			want:   WantImage,
			target: "release",
			error: `"release" is a publication(), which publishes images to repositories; ` +
				`"vamos publish release --tag ..." publishes it, and "vamos build release" builds them`,
		},
		{
			name:   "export a publication's files",
			want:   WantFiles,
			target: "release",
			error: `"release" is a publication(), which publishes images to repositories; ` +
				`"vamos publish release --tag ..." publishes it, and "vamos build release" builds them`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadRelease(t, tc.want, nil, tc.target)
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Errorf("error = %v, want %q", err, tc.error)
			}
		})
	}
}
