package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/load"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"

	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
)

// update pins the pulls that names name, or every pull of the workspace, to what their tags resolve to now, at their
// own registries, rewriting the lockfile, and reports each pin it added, changed or removed, then each image's newer
// releases, its newer prerelease, or that its tag isn't a version.
// A full update removes the pins that no pull uses; naming pulls changes only theirs.
// A pull whose image or tag is a function of ctx is pinned in the configuration that building it would give it, for
// --platform and the execution platform, and a full update then removes no pin, since another configuration of it
// may use one.
// Nothing is written unless every pull resolves; with --dry-run, nothing is written at all, and the command fails if
// the lockfile would change.
func (inv *invocation) update(names []string) error {
	exec, err := inv.updateExecPlatform()
	if err != nil {
		return err
	}
	pulls, root, unsure, err := load.Pulls(inv.ctx, inv.app.fs, inv.request(names, load.WantBuild, exec))
	if err != nil {
		return err
	}
	reg, err := inv.registry()
	if err != nil {
		return err
	}
	pins, newers, err := resolvePins(inv.ctx, reg, pulls)
	if err != nil {
		return err
	}
	remove := len(names) == 0
	var keptFor string // why the pins that no pull found uses may be used all the same, if they may
	if i := slices.IndexFunc(pulls, func(p load.Pull) bool { return p.Configured }); i >= 0 {
		keptFor = fmt.Sprintf("pull %q may in another configuration", pulls[i].Label)
	} else if unsure != "" {
		keptFor = fmt.Sprintf("%q, which couldn't be configured as building it would, may use a pull that does",
			unsure)
	}
	if keptFor != "" {
		remove = false
	}
	var changes []pinChange
	change := func(l *load.Lock) {
		changes = changePins(l, pins, remove)
		if len(names) == 0 && keptFor != "" {
			changes = append(changes, keptPins(l, pins, keptFor)...)
		}
	}
	var changed bool // whether the lockfile was written, or would be, with --dry-run
	if inv.opts.dryRun {
		changed, err = load.WouldUpdateLock(inv.app.fs, root, change)
	} else {
		changed, err = load.UpdateLock(inv.ctx, inv.app.fs, root, change)
	}
	if err != nil {
		return err
	}
	if err := inv.reportPins(changes, changed); err != nil {
		return err
	}
	if err := inv.reportNewer(newers); err != nil {
		return err
	}
	if inv.opts.dryRun && changed {
		return fmt.Errorf("%s would change; run vamos update", load.LockFile)
	}
	return nil
}

// reportNewer prints a line for each image of newers, sorted, that has newer releases, a newer prerelease, a tag
// that isn't a version, or a repository whose tags couldn't be listed, saying what.
func (inv *invocation) reportNewer(newers map[string]newer) error {
	for _, image := range slices.Sorted(maps.Keys(newers)) {
		n := newers[image]
		var releases []string
		switch {
		case n.minorKind == "final":
			releases = append(releases, fmt.Sprintf("its final release, %s", n.minor))
		case n.minor != "":
			releases = append(releases, fmt.Sprintf("a newer %s release, %s", n.minorKind, n.minor))
		}
		if n.major != "" {
			releases = append(releases, fmt.Sprintf("a newer major release, %s", n.major))
		}
		var err error
		if len(releases) > 0 {
			err = inv.printf("%s has %s\n", image, strings.Join(releases, ", and "))
		}
		if err == nil && n.prerelease != "" {
			err = inv.printf("%s has a newer prerelease, %s\n", image, n.prerelease)
		}
		if err == nil && n.unlisted != nil {
			err = inv.printUnlisted(image, n.unlisted)
		}
		if err == nil && n.notVersion {
			err = inv.printf("%s isn't tagged with a version, so newer releases of it can't be reported\n", image)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// resolvePins returns the pin of each image that pulls pull, resolved at its registry, reg: each repository's tags
// listed once, each tag resolved to the newest tag it covers, and that tag's digest; and what to report of each
// image's newer releases. A repository whose tags can't be listed has each tag resolve to itself, noted.
func resolvePins(ctx context.Context, reg Registry, pulls []load.Pull) (map[string]load.Pin, map[string]newer,
	error) {
	pins, newers := map[string]load.Pin{}, map[string]newer{}
	listed := map[string][]string{} // the tags of each repository listed so far, by name
	unlisted := map[string]error{}  // why each repository whose tags couldn't be listed couldn't be, by name
	for _, p := range pulls {
		if _, ok := pins[p.Image]; ok {
			continue
		}
		named, err := reference.ParseNamed(p.Image)
		if err != nil {
			return nil, nil, fmt.Errorf("pull %q: %w", p.Label, err) // a bug: load gives normalized images
		}
		tagged := named.(reference.NamedTagged)
		repo := reference.TrimNamed(named)
		tags, ok := listed[repo.Name()]
		_, isVersion := parseVersion(tagged.Tag())
		if !ok && unlisted[repo.Name()] == nil && isVersion {
			// A registry that refuses to list tags still says what a tag names, so the tag resolves to itself;
			// any other failure, such as an unreachable registry, fails the update.
			listing, err := reg.Tags(ctx, repo)
			switch {
			case err == nil:
				tags, listed[repo.Name()] = listing, listing
			case listingRefused(err):
				unlisted[repo.Name()] = err
			case imageUnknown(err):
				return nil, nil, fmt.Errorf("resolving %s for pull %q: listing its tags: %w (check the name, and "+
					"log in if the image is private)", tagged, p.Label, err)
			default:
				return nil, nil, fmt.Errorf("resolving %s for pull %q: listing its tags: %w", tagged, p.Label, err)
			}
		}
		resolved, err := reference.WithTag(repo, resolveTag(tagged.Tag(), tags))
		if err != nil {
			return nil, nil, fmt.Errorf("pull %q: %w", p.Label, err)
		}
		d, err := reg.Digest(ctx, resolved)
		if err != nil {
			hint := ""
			if listingRefused(err) || imageUnknown(err) {
				hint = " (check the name, and log in if the image is private)"
			}
			return nil, nil, fmt.Errorf("resolving %s for pull %q: %w%s", resolved, p.Label, err, hint)
		}
		if d.Algorithm() != digest.SHA256 {
			return nil, nil, fmt.Errorf("resolving %s for pull %q: the registry names it by %s, but pins are sha256 "+
				"digests, which pulling verifies the manifest by", resolved, p.Label, d)
		}
		pins[p.Image] = load.Pin{Tag: resolved.Tag(), Digest: d}
		if err := unlisted[repo.Name()]; err != nil && isVersion {
			newers[p.Image] = newer{unlisted: err}
		} else {
			newers[p.Image] = newerReleases(tagged.Tag(), resolved.Tag(), tags)
		}
	}
	return pins, newers, nil
}

// pinChange is a pin that updating added, changed or removed: before, after, or both;
// or one that it kept, though no pull found uses it here, for the reason keptFor gives.
type pinChange struct {
	image         string
	before, after *load.Pin
	keptFor       string
}

// keptPins returns the pins of l that pins doesn't have, which a full update keeps, for the reason keptFor: that a
// pull whose image or tag is a function of ctx may use them in another configuration than this machine's, or that a
// target that couldn't be configured may use a pull that does.
func keptPins(l *load.Lock, pins map[string]load.Pin, keptFor string) []pinChange {
	var kept []pinChange
	for _, image := range slices.Sorted(maps.Keys(l.Pulls)) {
		if _, ok := pins[image]; !ok {
			before := l.Pulls[image]
			kept = append(kept, pinChange{image: image, before: &before, keptFor: keptFor})
		}
	}
	return kept
}

// changePins sets l's pins to pins, and, if full, removes those that pins doesn't have, returning what changed,
// sorted by image.
func changePins(l *load.Lock, pins map[string]load.Pin, full bool) []pinChange {
	var changes []pinChange
	for _, image := range slices.Sorted(maps.Keys(pins)) {
		after := pins[image]
		before, ok := l.Pulls[image]
		switch {
		case !ok:
			changes = append(changes, pinChange{image: image, after: &after})
		case before != after:
			changes = append(changes, pinChange{image: image, before: &before, after: &after})
		}
		l.Pulls[image] = after
	}
	if full {
		for _, image := range slices.Sorted(maps.Keys(l.Pulls)) {
			if _, ok := pins[image]; !ok {
				before := l.Pulls[image]
				changes = append(changes, pinChange{image: image, before: &before})
				delete(l.Pulls, image)
			}
		}
		slices.SortFunc(changes, func(a, b pinChange) int { return strings.Compare(a.image, b.image) })
	}
	return changes
}

// reportPins prints a line for each change to the lockfile, or that there was none, and each pin kept;
// a lockfile rewritten with the same pins, as this vamos writes it, is reported as such.
// changed says whether the lockfile was written, or would be; with --dry-run, each line says what would change.
func (inv *invocation) reportPins(changes []pinChange, changed bool) error {
	would := func(verb, conditional string) string {
		if inv.opts.dryRun {
			return conditional
		}
		return verb
	}
	if !slices.ContainsFunc(changes, func(c pinChange) bool { return c.keptFor == "" }) {
		status := "is up to date"
		if changed {
			status = would("pins the same, rewritten as this vamos writes it",
				"pins the same, but would be rewritten as this vamos writes it")
		}
		if err := inv.printf("%s %s\n", load.LockFile, status); err != nil {
			return err
		}
	}
	for _, c := range changes {
		var err error
		switch {
		case c.keptFor != "":
			err = inv.printf("%s %s, which no pull uses here, since %s\n", would("kept", "keeps"), c.image, c.keptFor)
		case c.before == nil:
			err = inv.printf("%s %s to %s\n", would("pinned", "would pin"), c.image, pinText(*c.after))
		case c.after == nil:
			err = inv.printf("%s %s, which no pull uses, from %s\n", would("removed", "would remove"), c.image,
				pinText(*c.before))
		default:
			err = inv.printf("%s %s from %s to %s\n", would("updated", "would update"), c.image,
				pinText(*c.before), pinText(*c.after))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pinText returns p as the report shows it: its tag and digest, as tag@digest.
func pinText(p load.Pin) string {
	return p.Tag + "@" + p.Digest.String()
}

// listingRefused reports whether err, from listing a repository's tags, says plainly that the registry refuses to list
// them, as one that forbids listing, or doesn't offer it, does: with the code DENIED, UNAUTHORIZED or UNSUPPORTED,
// or a 401, 403, 404 or 405 without a code. Anything else, an unreachable registry, a 429, a 5xx or a code of the
// registry's own, whose status containers/image doesn't keep, fails the update rather than change a pin on a failure
// that may pass.
func listingRefused(err error) bool {
	var unauthorized docker.ErrUnauthorizedForCredentials
	if errors.As(err, &unauthorized) {
		return true
	}
	var coded errcode.ErrorCoder // errcode.Error, or errcode.ErrorCode for a code without a message
	var unexpected docker.UnexpectedHTTPStatusError
	status := 0
	switch {
	case errors.As(err, &coded):
		switch coded.ErrorCode() {
		case errcode.ErrorCodeDenied, errcode.ErrorCodeUnauthorized, errcode.ErrorCodeUnsupported:
			return true
		}
		return false
	case errors.As(err, &unexpected):
		status = unexpected.StatusCode
	default:
		// containers/image reports a 4xx without an error code by a type of its own, which it doesn't export,
		// as "StatusCode: 403, ...".
		if m := unexportedStatus.FindStringSubmatch(err.Error()); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

// unexportedStatus matches the status of an answer without an error code, as containers/image reports one.
var unexportedStatus = regexp.MustCompile(`StatusCode: (\d{3}),`)

// imageUnknown reports whether err says that the registry has no such repository or manifest.
func imageUnknown(err error) bool {
	var coded errcode.ErrorCoder
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.ErrorCode() {
	case v2.ErrorCodeNameUnknown, v2.ErrorCodeManifestUnknown:
		return true
	}
	return false
}

// updateExecPlatform returns the execution platform that update configures pulls for, which ctx.exec_platform names,
// as building would: the one a coordinator, with --coordinator, says to use, asking it for nothing else, or else the
// one --exec-platform names, or else this machine's, which the local executor runs commands on.
func (inv *invocation) updateExecPlatform() (imgspecv1.Platform, error) {
	if inv.opts.coordinator != "" {
		client, err := remote.NewClient(inv.opts.coordinator)
		if err != nil {
			return imgspecv1.Platform{}, fmt.Errorf("--coordinator: %w", err)
		}
		defer client.Close()
		caps, err := client.Capabilities(inv.ctx) // whatever graphs it runs, since update submits none
		if err != nil {
			return imgspecv1.Platform{}, err
		}
		return inv.execPlatform(caps)
	}
	if inv.opts.execPlatform != "" {
		return inv.givenExecPlatform()
	}
	return run.HostPlatform, nil
}

// printUnlisted says that image is pinned to what its tag names, rather than the newest tag it covers, since its tags
// couldn't be listed, as err says.
func (inv *invocation) printUnlisted(image string, err error) error {
	return inv.printf("%s is pinned to what its tag names, since its tags couldn't be listed: %v\n", image, err)
}
