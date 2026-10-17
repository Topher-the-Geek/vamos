package fileset

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/spf13/afero"
)

// Defaults of a selection.
// The limits catch a pattern that reaches much further than meant,
// such as into node_modules, before all of its files are read.
const (
	defaultModeMask     = 0o755
	defaultMaxFileCount = 10_000
	defaultMaxFileSize  = 100 << 20 // 100 MiB
	defaultMaxTotalSize = 1 << 30   // 1 GiB
)

// Selection says which files a glob() or a task's output holds:
// those within a directory that its include patterns match, less those its exclude patterns match.
// It is part of a task's action, so its fields are encoded as the action is.
type Selection struct {
	Include      []string `json:"include"`           // cleaned patterns
	Exclude      []string `json:"exclude,omitempty"` // cleaned patterns
	ModeMask     int      `json:"mode_mask"`         // chmod bits kept of each file's and directory's mode
	AllowNoFiles bool     `json:"allow_no_files,omitempty"`
	MaxFileCount int      `json:"max_file_count"` // files and links
	MaxFileSize  int64    `json:"max_file_size"`  // bytes
	MaxTotalSize int64    `json:"max_total_size"` // bytes

	include, exclude []pattern // parsed by compile
}

// pattern is an include or exclude pattern, relative to its selection's directory.
// Its parts are matched against a path's parts with path.Match,
// but for "**", which matches any number of parts, even none.
type pattern struct {
	text   string   // cleaned, for messages
	parts  []string // a path without wildcards ends in "**", so that it also matches everything beneath it
	prefix string   // its leading parts without wildcards, or "." for none: the directory its files are beneath
}

// DefaultSelection returns a selection with the default mode mask and limits, which has no patterns yet.
func DefaultSelection() *Selection {
	return &Selection{
		ModeMask:     defaultModeMask,
		MaxFileCount: defaultMaxFileCount,
		MaxFileSize:  defaultMaxFileSize,
		MaxTotalSize: defaultMaxTotalSize,
	}
}

// Compile checks sel's patterns, relative to dir, mode mask and limits, cleans its patterns and parses them.
func (sel *Selection) Compile(dir string) error {
	switch {
	case sel.ModeMask < 0 || sel.ModeMask > 0o7777:
		return fmt.Errorf("mode_mask %O must be between 0 and 0o7777", sel.ModeMask)
	case sel.MaxFileCount < 1:
		return fmt.Errorf("max_file_count must be positive, not %d", sel.MaxFileCount)
	case sel.MaxFileSize < 1:
		return fmt.Errorf("max_file_size must be positive, not %d", sel.MaxFileSize)
	case sel.MaxTotalSize < 1:
		return fmt.Errorf("max_total_size must be positive, not %d", sel.MaxTotalSize)
	}
	var err error
	if sel.include, err = parsePatterns("include", sel.Include, dir); err != nil {
		return err
	}
	if sel.exclude, err = parsePatterns("exclude", sel.Exclude, dir); err != nil {
		return err
	}
	// Keep the cleaned patterns, sorted and once each,
	// so that a task's key is the same however they were written and in whatever order:
	// neither changes which files they select.
	sel.Include, sel.Exclude = patternTexts(sel.include), patternTexts(sel.exclude)
	return nil
}

// parsePatterns parses the patterns of the parameter param.
func parsePatterns(param string, texts []string, dir string) ([]pattern, error) {
	patterns := make([]pattern, len(texts))
	for i, text := range texts {
		if text == "" {
			return nil, fmt.Errorf(`%s[%d] is empty; use "." for the whole directory`, param, i)
		}
		p, err := parsePattern(param, text, dir)
		if err != nil {
			return nil, err
		}
		patterns[i] = p
	}
	return patterns, nil
}

// patternTexts returns the cleaned texts of patterns, sorted and once each, or nil if there are none.
func patternTexts(patterns []pattern) []string {
	var texts []string
	for _, p := range patterns {
		texts = append(texts, p.text)
	}
	slices.Sort(texts)
	return slices.Compact(texts)
}

// parsePattern parses text, a pattern of the parameter param, relative to dir.
func parsePattern(param, text, dir string) (pattern, error) {
	if err := checkRelative(param, text, dir); err != nil {
		return pattern{}, err
	}
	p := pattern{text: path.Clean(text)}
	if p.text != "." {
		p.parts = strings.Split(p.text, "/")
	}
	literal, wild := 0, false
	for _, part := range p.parts {
		if part != "**" && strings.Contains(part, "**") {
			return pattern{}, fmt.Errorf(`%s %q: "**" must be a whole part of the path, as in "src/**/*.ts"`,
				param, text)
		}
		// Match checks the whole pattern, even when the name doesn't match.
		if _, err := path.Match(part, ""); err != nil {
			return pattern{}, fmt.Errorf("%s %q: %w", param, text, err)
		}
		if wild = wild || strings.ContainsAny(part, `*?[\`); !wild {
			literal++
		}
	}
	p.prefix = path.Join(append([]string{"."}, p.parts[:literal]...)...)
	if literal == len(p.parts) {
		p.parts = append(p.parts, "**")
	}
	return p, nil
}

// matchParts reports whether the parts of a path match the parts of a pattern.
func matchParts(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := range len(name) + 1 {
				if matchParts(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], name[0]); !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// mayMatchBeneath reports whether the parts of a pattern could match a path beneath the directory with the given parts,
// so that the walk needs to read it.
func mayMatchBeneath(pattern, dir []string) bool {
	for ; len(dir) > 0; pattern, dir = pattern[1:], dir[1:] {
		if len(pattern) == 0 {
			return false
		}
		if pattern[0] == "**" {
			return true
		}
		if ok, _ := path.Match(pattern[0], dir[0]); !ok {
			return false
		}
	}
	return len(pattern) > 0
}

// pathParts splits a cleaned relative path into its parts, none for ".".
func pathParts(rel string) []string {
	if rel == "." {
		return nil
	}
	return strings.Split(rel, "/")
}

// excluded reports whether an exclude pattern matches the path with the given parts.
func (sel *Selection) excluded(parts []string) bool {
	return slices.ContainsFunc(sel.exclude, func(p pattern) bool { return matchParts(p.parts, parts) })
}

// excludedWithin reports whether an exclude pattern matches the path with the given parts or a directory that holds it.
// The walk skips an excluded directory and all it holds, so a walk that starts beneath one must skip it too.
func (sel *Selection) excludedWithin(parts []string) bool {
	for i := range parts {
		if sel.excluded(parts[:i+1]) {
			return true
		}
	}
	return false
}

// matchPrefix returns the outermost prefix of the include patterns that match the path with the given parts,
// or false if none does.
// The prefixes of patterns that match one path all hold it, so each holds the others or is within them.
func (sel *Selection) matchPrefix(parts []string) (string, bool) {
	prefix, matched := "", false
	for _, p := range sel.include {
		if matchParts(p.parts, parts) && (!matched || within(prefix, p.prefix)) {
			prefix, matched = p.prefix, true
		}
	}
	return prefix, matched
}

// treeWalk is a walk of the files a selection selects within a directory, which Walk describes.
type treeWalk struct {
	ctx      context.Context // the walk stops between directories when it ends
	fsys     afero.Fs
	dir      string
	sel      *Selection
	file     func(name string, limit int64) (cache.Blob, error)
	dirModes map[string]uint32 // of the directories read but not yet recorded, by path relative to dir
	entries  []cache.TreeEntry
	files    int   // files and links selected
	total    int64 // size of the files selected
}

// Walk describes the files that sel selects within dir in fsys as a tree,
// passing each file's name to file for its blob, with the most bytes the limits leave for it.
// file fails with ErrOverLimit if the file holds more.
// It starts at each include pattern's prefix,
// and reads only directories that some include pattern could match within and no exclude pattern matches.
// Directories beneath a prefix that hold selected files are recorded too, with their modes,
// but not those at or above it, which the image keeps as they are.
//
// The walk doesn't follow links within dir: they are recorded as links, as Docker's COPY does,
// so they can't bring in files from outside it.
// checkInclude refuses links on the way to each prefix.
// dir itself may be a link, as a project reached through a linked $PWD is.
// The walk stops with ctx's error, between entries, once ctx ends.
func Walk(
	ctx context.Context, fsys afero.Fs, dir string, sel *Selection,
	file func(name string, limit int64) (cache.Blob, error),
) (*cache.Tree, error) {
	w := &treeWalk{
		ctx:      ctx,
		fsys:     fsys,
		dir:      dir,
		sel:      sel,
		file:     file,
		dirModes: map[string]uint32{},
	}
	var prefixes []string
	for _, p := range sel.include {
		if err := checkInclude(fsys, dir, p); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, p.prefix)
	}
	// Walk from the outermost prefixes, since the walk from each reaches any prefix beneath it.
	// Shorter paths come first, as "." must, though names such as "-x" sort before it.
	slices.SortFunc(prefixes, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	})
	var roots []string
	for _, prefix := range slices.Compact(prefixes) {
		if !slices.ContainsFunc(roots, func(root string) bool { return within(prefix, root) }) {
			roots = append(roots, prefix)
		}
	}
	for _, root := range roots {
		if sel.excludedWithin(pathParts(root)) {
			continue
		}
		var info fs.FileInfo
		var err error
		if root == "." {
			info, err = fsys.Stat(dir)
		} else {
			info, _, err = lstat(fsys, filepath.Join(dir, root))
		}
		if err != nil {
			return nil, err
		}
		if err := w.visit(root, info); err != nil {
			return nil, err
		}
	}
	if w.files == 0 && !sel.AllowNoFiles {
		subject := "include " + QuoteNames(sel.Include)
		if len(sel.Exclude) > 0 {
			subject += " but not exclude " + QuoteNames(sel.Exclude)
		}
		return nil, fmt.Errorf("no files in %s match %s; pass allow_no_files=True if that's expected", dir, subject)
	}
	w.recordBetween()
	slices.SortFunc(w.entries, func(a, b cache.TreeEntry) int { return strings.Compare(a.Path, b.Path) })
	return &cache.Tree{Entries: w.entries}, nil
}

// visit walks the file at rel, relative to the walk's directory, which info describes.
func (w *treeWalk) visit(rel string, info fs.FileInfo) error {
	parts := pathParts(rel)
	if w.sel.excluded(parts) {
		return nil
	}
	src := filepath.Join(w.dir, rel)
	if info.IsDir() {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if !slices.ContainsFunc(w.sel.include, func(p pattern) bool { return mayMatchBeneath(p.parts, parts) }) {
			return nil
		}
		w.dirModes[rel] = w.mode(info)
		names, err := readDirNames(w.fsys, src)
		if err != nil {
			return err
		}
		slices.Sort(names) // so that a limit is reported at the same file every time
		for _, name := range names {
			info, _, err := lstat(w.fsys, filepath.Join(src, name))
			if err != nil {
				return err
			}
			if err := w.visit(path.Join(rel, name), info); err != nil {
				return err
			}
		}
		return nil
	}

	prefix, ok := w.sel.matchPrefix(parts)
	if !ok {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.files++
	if w.files > w.sel.MaxFileCount {
		return fmt.Errorf("more than max_file_count=%d files in %s match; exclude some, or raise the limit",
			w.sel.MaxFileCount, w.dir)
	}
	entry := cache.TreeEntry{Path: rel}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		reader, ok := w.fsys.(afero.LinkReader)
		if !ok {
			return fmt.Errorf("%s: cannot read symbolic links", src)
		}
		target, err := reader.ReadlinkIfPossible(src)
		if err != nil {
			return err
		}
		entry.Type, entry.Target = cache.EntryLink, target
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file, directory or symbolic link", src)
	default:
		// Check the size before reading the file, and read no more than the limits allow, in case it grew meanwhile.
		if info.Size() > w.sel.MaxFileSize {
			return fmt.Errorf("%s is %d bytes, more than max_file_size=%d", src, info.Size(), w.sel.MaxFileSize)
		}
		left := w.sel.MaxTotalSize - w.total
		if info.Size() > left {
			return w.overTotal()
		}
		b, err := w.file(src, min(w.sel.MaxFileSize, left))
		if errors.Is(err, cache.ErrOverLimit) {
			if w.sel.MaxFileSize < left {
				return fmt.Errorf("%s grew to more than max_file_size=%d bytes as it was read", src, w.sel.MaxFileSize)
			}
			return w.overTotal()
		} else if err != nil {
			return err
		}
		w.total += b.Size
		entry.Type, entry.Mode, entry.Blob = cache.EntryFile, w.mode(info), &b
	}
	w.entries = append(w.entries, entry)
	// Record the directories that hold the file beneath the prefix, which were read on the way to it.
	// Go on past those already recorded:
	// a file matched by a pattern with a deeper prefix recorded only those beneath it.
	for d := path.Dir(rel); d != prefix && within(d, prefix); d = path.Dir(d) {
		if mode, ok := w.dirModes[d]; ok {
			delete(w.dirModes, d)
			w.entries = append(w.entries, cache.TreeEntry{Path: d, Type: cache.EntryDir, Mode: mode})
		}
	}
	return nil
}

// recordBetween records the directories between each recorded directory and the entries within it.
// A recorded directory is copied with all it holds, so those between must be recorded too, with their modes,
// even when they are at or above the prefix of the pattern that selected the entries:
// otherwise they would be copied as the temporary directories that hold the entries when they are checked out.
func (w *treeWalk) recordBetween() {
	recorded := map[string]bool{}
	for _, e := range w.entries {
		if e.Type == cache.EntryDir {
			recorded[e.Path] = true
		}
	}
	// Each recorded directory is an entry too, so it records those between it and the next one out.
	for _, e := range slices.Clone(w.entries) {
		var between []string
		for d := path.Dir(e.Path); d != "."; d = path.Dir(d) {
			if recorded[d] {
				for _, b := range between {
					recorded[b] = true
					w.entries = append(w.entries, cache.TreeEntry{Path: b, Type: cache.EntryDir, Mode: w.dirModes[b]})
				}
				break
			}
			between = append(between, d)
		}
	}
}

// overTotal returns the error for files that total more than the selection's max_total_size.
func (w *treeWalk) overTotal() error {
	return fmt.Errorf("the files in %s that match total more than max_total_size=%d bytes", w.dir, w.sel.MaxTotalSize)
}

// mode returns the chmod bits of info's mode that the selection keeps.
func (w *treeWalk) mode(info fs.FileInfo) uint32 {
	return cache.ChmodBits(info) & uint32(w.sel.ModeMask)
}

// Stats describes the files of a selection, as read and stored.
type Stats struct {
	cache.Count
	NewFiles int   // files whose content the cache lacked, and so stored
	NewBytes int64 // their size
}

// Args returns s as a trace's arguments: a list of keys and values.
func (s Stats) Args() []any {
	return append(s.Count.Args(), "new files", s.NewFiles, "new bytes", s.NewBytes)
}

// Hash returns the blob that the tree of the files sel selects within dir in fsys would be stored as,
// and their stats, storing nothing.
func Hash(ctx context.Context, fsys afero.Fs, dir string, sel *Selection) (cache.Blob, Stats, error) {
	t, err := Walk(ctx, fsys, dir, sel, func(name string, limit int64) (cache.Blob, error) {
		return cache.HashFile(fsys, name, limit)
	})
	if err != nil {
		return cache.Blob{}, Stats{}, err
	}
	return t.Digest(), Stats{Count: t.Count()}, nil
}

// storeFiles stores the files that sel selects within dir in fsys in store,
// and returns the tree that describes them, without storing it, and their stats.
func storeFiles(ctx context.Context, store cache.ContentStore, fsys afero.Fs, dir string, sel *Selection) (
	*cache.Tree, Stats, error) {
	var stats Stats
	t, err := Walk(ctx, fsys, dir, sel, func(name string, limit int64) (cache.Blob, error) {
		b, stored, err := cache.PutFile(ctx, store, fsys, name, limit)
		if stored {
			stats.NewFiles++
			stats.NewBytes += b.Size
		}
		return b, err
	})
	if err != nil {
		return nil, Stats{}, err
	}
	stats.Count = t.Count()
	return t, stats, nil
}

// Store stores the files that sel selects within dir in fsys in store,
// and then the tree that describes them, and returns the tree's blob and the files' stats.
func Store(ctx context.Context, store cache.ContentStore, fsys afero.Fs, dir string, sel *Selection) (
	cache.Blob, Stats, error) {
	t, stats, err := storeFiles(ctx, store, fsys, dir, sel)
	if err != nil {
		return cache.Blob{}, Stats{}, err
	}
	b, err := store.PutBytes(ctx, t.Encode())
	return b, stats, err
}

// checkInclude reports whether the prefix of the include pattern p,
// the path its files are beneath, is within dir, reached without following symbolic links.
// The prefix must exist, which catches a misspelled path, but the pattern needn't match anything.
func checkInclude(fsys afero.Fs, dir string, p pattern) error {
	if p.prefix == "." {
		// dir itself was already found, and may be reached through a link, as from a linked $PWD.
		return nil
	}
	subject := fmt.Sprintf("include %q", p.text)
	if p.prefix != p.text {
		subject += fmt.Sprintf(": %q", p.prefix)
	}
	// Check each part: lstat on the final path still follows parent links.
	current := dir
	for _, part := range strings.Split(p.prefix, "/") {
		current = filepath.Join(current, part)
		info, _, err := lstat(fsys, current)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s not found in %s", subject, dir)
		} else if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if current == filepath.Join(dir, p.prefix) {
				return fmt.Errorf("%s is a symbolic link; include what it links to instead", subject)
			}
			return fmt.Errorf("%s has symbolic link parent %q", subject, current)
		}
	}
	return nil
}

// checkRelative reports whether p, a path or pattern of the parameter param, is relative and stays within dir.
func checkRelative(param, p, dir string) error {
	if path.IsAbs(p) {
		return fmt.Errorf("%s %q must be a path relative to %s", param, p, dir)
	}
	if clean := path.Clean(p); clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s %q is outside %s", param, p, dir)
	}
	return nil
}

// within reports whether the cleaned relative path p is dir or inside it.
func within(p, dir string) bool {
	return dir == "." || p == dir || strings.HasPrefix(p, dir+"/")
}

// QuoteNames lists names, quoted, for a message.
func QuoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ")
}

// lstat describes the file at path in fsys without following a final symbolic link, if fsys supports links.
func lstat(fsys afero.Fs, path string) (fs.FileInfo, bool, error) {
	if l, ok := fsys.(afero.Lstater); ok {
		return l.LstatIfPossible(path)
	}
	info, err := fsys.Stat(path)
	return info, false, err
}

// readDirNames returns the names of the entries in the directory dir in fsys.
func readDirNames(fsys afero.Fs, dir string) ([]string, error) {
	f, err := fsys.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
