package load

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
)

// Want says what a command does with the targets it names, which decides the kinds of target it accepts.
type Want int

const (
	WantBuild Want = iota // build them into the build cache: pulls, layers, tasks and indexes
	WantImage             // send the image of one: a pull, layer or index
	WantFiles             // export the files of one: a task given source
)

// Request asks Load for the graph of building the targets a command names.
type Request struct {
	Dir          string   // -C: the directory of the BUILD file whose targets are named without a label
	Targets      []string // as the command line names them
	Want         Want
	Sets         []string           // --set flags, each name=value
	Platform     string             // --platform, or "" for the executor's
	ExecPlatform imgspecv1.Platform // the executor's, which EXEC_PLATFORM names
	// DestPlatform, if set, says that the image is sent to a destination that takes one image,
	// and for which platform, with why, so that an index sends only its manifest for that platform.
	// Its error is returned as it is.
	DestPlatform func(context.Context) (imgspecv1.Platform, string, error)
	// Store reads the files that a glob selects within dir in fsys, as it returns their tree, with their stats:
	// into the build cache, for a build.
	// It is called only once the targets are configured, so that a mistake in a BUILD file touches nothing.
	Store func(fsys afero.Fs, dir string, sel *fileset.Selection) (cache.Blob, fileset.Stats, error)
}

// Load evaluates the BUILD files of the workspace holding req.Dir, as the targets req names need them,
// configures those targets, reads the files of the globs they add with req.Store, and returns their graph, checked.
// Every mistake in a BUILD file or the command line is reported here, saying where.
// Loading is one span of ctx's trace, and reading each glob another, within it.
func Load(ctx context.Context, fsys afero.Fs, req Request) (_ *graph.Graph, err error) {
	ctx, span := tracing.Start(ctx, "load", "targets", req.Targets)
	defer func() { span.EndWith(err) }()
	g, w, err := load(ctx, fsys, req)
	if w != nil {
		span.Set("workspace", w.root, "BUILD files", len(w.packages))
	}
	if g != nil {
		span.Set("nodes", len(g.Nodes))
	}
	return g, err
}

// load is Load, also returning the workspace, once opened.
func load(ctx context.Context, fsys afero.Fs, req Request) (*graph.Graph, *workspace, error) {
	dir, err := filepath.Abs(req.Dir)
	if err != nil {
		return nil, nil, err
	}
	w, err := openWorkspace(fsys, dir, req.ExecPlatform)
	if err != nil {
		return nil, nil, err
	}
	if req.Platform != "" {
		if w.platform, err = parsePlatform(req.Platform); err != nil {
			return nil, w, fmt.Errorf("--platform %s: %w", req.Platform, err)
		}
	}

	var roots []confLabel
	switch req.Want {
	case WantImage:
		root, err := w.imageRoot(ctx, req)
		if err != nil {
			return nil, w, err
		}
		roots = []confLabel{root}
	case WantFiles:
		l, err := w.filesTarget(req.Targets[0])
		if err != nil {
			return nil, w, err
		}
		c, err := w.commandConfigOf(l, "task", req, false)
		if err != nil {
			return nil, w, err
		}
		roots = []confLabel{{l, c}}
	default:
		if roots, err = w.buildRoots(req); err != nil {
			return nil, w, err
		}
	}

	g, globs, err := w.configure(roots)
	if err != nil {
		return nil, w, err
	}
	err = w.readGlobs(ctx, g, globs, func(dir string, sel *fileset.Selection) (cache.Blob, fileset.Stats, error) {
		return req.Store(w.fs, dir, sel)
	})
	if err != nil {
		return nil, w, err
	}
	if err := g.Validate(); err != nil {
		return nil, w, err // a bug: every mistake in a BUILD file is reported before this
	}
	return g, w, nil
}

// imageRoot returns the target whose image req sends, in the configuration the command line gives it: the one named,
// or, if it is an index and the destination takes one image, its manifest for the destination's platform.
func (w *workspace) imageRoot(ctx context.Context, req Request) (confLabel, error) {
	l, kind, err := w.imageTarget(req.Targets[0])
	if err != nil {
		return confLabel{}, err
	}
	c, err := w.commandConfigOf(l, kind, req, false)
	if err != nil {
		return confLabel{}, err
	}
	root := confLabel{l, c}
	if kind != "index" || req.DestPlatform == nil {
		return root, nil
	}
	p, why, err := req.DestPlatform(ctx)
	if err != nil {
		return confLabel{}, err
	}
	manifests, err := w.indexManifests(root)
	if err != nil {
		return confLabel{}, err
	}
	return w.chooseManifest(l, manifests, p, why)
}

// buildRoots returns the targets req names to build, each in the configuration the command line gives it.
func (w *workspace) buildRoots(req Request) ([]confLabel, error) {
	labels, kinds, err := w.buildTargets(req.Targets)
	if err != nil {
		return nil, err
	}
	// Check every target's arguments before configuring any.
	// Each takes the --set flags naming its parameters.
	shared := len(labels) > 1
	roots := make([]confLabel, len(labels))
	for i, l := range labels {
		c, err := w.commandConfigOf(l, kinds[i], req, shared)
		if err != nil {
			return nil, err
		}
		roots[i] = confLabel{l, c}
	}
	if shared {
		if err := w.checkSets(labels, req.Sets); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

// commandConfigOf returns the configuration that the command line gives the target with label l and kind,
// one of several named if shared.
func (w *workspace) commandConfigOf(l, kind string, req Request, shared bool) (*config, error) {
	if kind == "index" && req.Platform != "" {
		return nil, fmt.Errorf("%q is an index(), whose manifests are each built for their own platform; "+
			"name one of them to build it for the platform given", w.display(l))
	}
	return w.commandConfig(l, req.Sets, shared)
}
