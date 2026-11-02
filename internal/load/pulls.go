package load

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/afero"
)

// Pull is a pull() target, and the image and tag it pulls, as the lockfile keys its pin; it is comparable.
type Pull struct {
	Label string // as messages name it: "base" or "//ui:base"
	Image string // normalized, with its tag, as graph.Pull.Image gives it
	// Configured says that its image or tag is a function of ctx, so that Image is the one it pulls in the
	// configuration that the command line gives it, and it may pull others in others.
	Configured bool
}

// Pulls returns the pulls of the workspace holding req.Dir, and its root, where its lockfile is:
// the targets that req.Targets names, which must be pulls, or, if it names none, every pull that a BUILD file of the
// workspace declares, each in the configuration that the command line gives it, as building it would.
// Every BUILD file is found by walking the workspace from its root, past hidden directories, symbolic links and other
// workspaces, those beneath it with a WORKSPACE file of their own; a package that a target found uses, even through a
// function of ctx, is found too, by configuring every target as building it would. unsure names a target, if any,
// that couldn't be configured so, whose functions of ctx may use pulls that weren't found.
// The lockfile is read, as loading reads it, so that a malformed one fails before anything else is done.
func Pulls(ctx context.Context, fsys afero.Fs, req Request) (pulls []Pull, root, unsure string, err error) {
	w, err := open(ctx, fsys, req)
	if err != nil {
		return nil, "", "", err
	}
	var labels []string
	if len(req.Targets) > 0 {
		for _, name := range req.Targets {
			l, kind, err := w.target(name)
			if err != nil {
				return nil, "", "", err
			}
			if kind != "pull" {
				return nil, "", "", fmt.Errorf("%q is %s %s(), and only pulls are updated", name, article(kind), kind)
			}
			if !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	} else if labels, unsure, err = w.everyPull(req); err != nil {
		return nil, "", "", err
	}
	pulls = make([]Pull, 0, len(labels))
	for _, l := range labels {
		c, err := w.commandConfigOf(l, "pull", req, false)
		if err != nil {
			return nil, "", "", err
		}
		d := w.declaration(l)
		tmpl, err := w.template(d, confLabel{l, c})
		if err != nil {
			return nil, "", "", err
		}
		pulls = append(pulls, Pull{Label: w.display(l), Image: tmpl.(*pullSpec).image.String(),
			Configured: d.static == nil})
	}
	return pulls, w.root, unsure, nil
}

// everyPull returns the labels of every pull that a BUILD file of the workspace declares, or that a package such a
// BUILD file uses declares, by package, then in the order declared; and a target, if any, that couldn't be configured
// as the command line req would configure it, so that a package its functions of ctx use may be missing.
// Every other target is configured so, as building it would, until no new package is loaded, so that a package used
// only through a function of ctx, as a hidden one may be, is found.
func (w *workspace) everyPull(req Request) (labels []string, unsure string, err error) {
	pkgs, err := w.packageDirs()
	if err != nil {
		return nil, "", err
	}
	for _, pkg := range pkgs {
		if _, err := w.load(pkg); err != nil {
			return nil, "", err
		}
	}
	cr := &configurer{w: w, pulls: map[confLabel]*pullSpec{}, targets: map[confLabel]*targetSpec{},
		globs: map[confLabel]*fileSet{}, indexes: map[confLabel][]confLabel{}}
	configured := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, pkg := range slices.Sorted(maps.Keys(w.packages)) {
			f := w.packages[pkg]
			for _, name := range f.names {
				l := label(pkg, name)
				if configured[l] {
					continue
				}
				configured[l], grew = true, true
				if err := w.configureTarget(cr, l, req); err != nil {
					if err := w.ctx.Err(); err != nil {
						return nil, "", err
					}
					if unsure == "" {
						unsure = w.display(l)
					}
				}
			}
		}
	}
	// Those of every package loaded, those the walk skips among them, as a hidden one that another uses.
	for _, pkg := range slices.Sorted(maps.Keys(w.packages)) {
		f := w.packages[pkg]
		for _, name := range f.names {
			if f.decls[name].kind == "pull" {
				labels = append(labels, label(pkg, name))
			}
		}
	}
	return labels, unsure, nil
}

// configureTarget configures the target l as the command line req would, as building it would, loading the packages
// that it uses; a pull or glob uses none.
func (w *workspace) configureTarget(cr *configurer, l string, req Request) error {
	kind := w.declaration(l).kind
	if kind == "pull" || kind == "glob" {
		return nil
	}
	if kind == "index" {
		req.Platform = "" // whose manifests are each configured for their own
	}
	c, err := w.commandConfigOf(l, kind, req, false)
	if err != nil {
		return err
	}
	roots := []confLabel{{l, c}}
	if kind == "publication" {
		if roots, _, err = w.expandPublication(roots[0]); err != nil {
			return err
		}
	}
	for _, cl := range roots {
		if err := cr.image(cl); err != nil {
			return err
		}
	}
	return nil
}

// packageDirs returns the packages of the workspace, sorted: its root, and the directories beneath it, that hold a
// BUILD file, itself a link or not, found without entering linked directories, though the root itself may be one, as
// a linked $PWD makes it, hidden directories, or directories that hold a WORKSPACE file, which are other workspaces.
func (w *workspace) packageDirs() ([]string, error) {
	var pkgs []string
	var walk func(pkg string) error
	walk = func(pkg string) error {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(w.root, filepath.FromSlash(pkg))
		entries, err := afero.ReadDir(w.fs, dir) // which describes each entry as itself, a link as a link
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			name := e.Name()
			switch {
			case !e.IsDir(): // a BUILD file may be a link, which the loader follows; a linked directory isn't entered
				found = found || slices.Contains(buildFileNames, name)
			case strings.HasPrefix(name, "."):
			default:
				sub := path.Join(pkg, name)
				if _, err := w.fs.Stat(filepath.Join(dir, name, "WORKSPACE")); err == nil {
					continue
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if err := walk(sub); err != nil {
					return err
				}
			}
		}
		if found {
			pkgs = append(pkgs, pkg)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, fmt.Errorf("looking for BUILD files in %s: %w", w.root, err)
	}
	slices.Sort(pkgs)
	return pkgs, nil
}
