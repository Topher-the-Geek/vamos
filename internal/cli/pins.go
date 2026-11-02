package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/load"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// The policies of --pins, for a pull that the lockfile doesn't pin.
const (
	pinsRecord  = "record"  // resolve it on the client and add its pin to the lockfile
	pinsResolve = "resolve" // leave it to the executor, which resolves it through its tag records
	pinsRequire = "require" // fail, before anything runs
)

// pinsPolicies are the values --pins and VAMOS_PINS take.
var pinsPolicies = []string{pinsRecord, pinsResolve, pinsRequire}

// pinsFlag is the value of --pins: what a command that builds does with a pull that the lockfile doesn't pin.
// Its default is VAMOS_PINS, where set, or else record; the flag outranks the variable.
type pinsFlag struct {
	policy string // as given, or "" for the default
}

func (f *pinsFlag) String() string { return f.policy }
func (f *pinsFlag) Type() string   { return "policy" }

// Set checks that value is a policy, so that a mistaken one is refused as the command line is parsed.
func (f *pinsFlag) Set(value string) error {
	if err := checkPinsPolicy(value); err != nil {
		return err
	}
	f.policy = value
	return nil
}

// checkPinsPolicy refuses a policy that isn't one of pinsPolicies, naming those there are.
func checkPinsPolicy(policy string) error {
	if !slices.Contains(pinsPolicies, policy) {
		return fmt.Errorf("%q isn't a policy for pulls without a pin; want record, resolve or require", policy)
	}
	return nil
}

// pinsPolicy returns the policy for pulls without a pin: --pins, or else VAMOS_PINS, or else record;
// and which gave it, as messages name it.
func (inv *invocation) pinsPolicy() (policy, givenBy string, err error) {
	if inv.opts.pins.policy != "" {
		return inv.opts.pins.policy, "--pins", nil
	}
	policy = os.Getenv("VAMOS_PINS")
	if policy == "" {
		return pinsRecord, "", nil
	}
	if err := checkPinsPolicy(policy); err != nil {
		return "", "", fmt.Errorf("VAMOS_PINS: %w", err)
	}
	return policy, "VAMOS_PINS", nil
}

// pinner returns what does what the pins policy says, --pins, VAMOS_PINS or record, which it checks now, before
// anything is loaded, with the pulls of a graph that the lockfile doesn't pin, as load.Request.Pin: see applyPins.
func (inv *invocation) pinner() (func(context.Context, *graph.Graph, string) error, error) {
	policy, givenBy, err := inv.pinsPolicy()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, g *graph.Graph, root string) error {
		return inv.applyPins(ctx, g, root, policy, givenBy)
	}, nil
}

// applyPins does what policy, which givenBy gave, says with the pulls of g, configured in the workspace whose root is
// root, that the lockfile doesn't pin, as load.Request.Pin is called, before any glob is read and anything runs:
// record resolves them at their own registries, as vamos update does, adds their pins to the lockfile, keeping any
// written meanwhile, reports each, and gives g their digests; resolve leaves them to the executor; require fails,
// naming every one of them and saying to run vamos update.
func (inv *invocation) applyPins(ctx context.Context, g *graph.Graph, root, policy, givenBy string) (err error) {
	if policy == pinsResolve {
		return nil
	}
	var unpinned []load.Pull // each pull once for each image, which a pull configured by ctx may have several of
	seen := map[load.Pull]bool{}
	for _, n := range g.Nodes {
		if n.Pull == nil || n.Pull.Digest != "" {
			continue
		}
		p := load.Pull{Label: n.Label, Image: n.Pull.Image}
		if !seen[p] {
			seen[p] = true
			unpinned = append(unpinned, p)
		}
	}
	if len(unpinned) == 0 {
		return nil
	}
	if policy == pinsRequire {
		named := make([]string, len(unpinned))
		for i, p := range unpinned {
			named[i] = fmt.Sprintf("%q (%s)", p.Label, p.Image)
		}
		return fmt.Errorf("%s=require: pulls without a pin in %s: %s; run vamos update to pin them",
			givenBy, load.LockFile, strings.Join(named, ", "))
	}
	ctx, span := tracing.Start(ctx, "record pins", "pulls", len(unpinned))
	defer func() { span.EndWith(err) }()
	reg, err := inv.registry()
	if err != nil {
		return err
	}
	pins, newers, err := resolvePins(ctx, reg, unpinned)
	switch {
	case err != nil && (imageUnknown(err) || listingRefused(err)):
		return err // which says to check the name
	case err != nil:
		return fmt.Errorf("%w; give --pins=resolve to resolve it on the executor, recording no pin", err)
	}
	var added []pinChange
	_, err = load.UpdateLock(ctx, inv.app.fs, root, func(l *load.Lock) {
		added = nil
		for _, image := range slices.Sorted(maps.Keys(pins)) {
			if pin, ok := l.Pulls[image]; ok { // pinned since the graph was loaded, as another build may have
				pins[image] = pin
				continue
			}
			pin := pins[image]
			l.Pulls[image] = pin
			added = append(added, pinChange{image: image, after: &pin})
		}
	})
	if err != nil {
		return fmt.Errorf("recording pins: %w; give --pins=resolve to build without recording them", err)
	}
	for _, n := range g.Nodes {
		if n.Pull != nil && n.Pull.Digest == "" {
			n.Pull.Digest = pins[n.Pull.Image].Digest
		}
	}
	for _, c := range added {
		if err := inv.printf("pinned %s to %s in %s\n", c.image, pinText(*c.after), load.LockFile); err != nil {
			return err
		}
		if n := newers[c.image]; n.unlisted != nil {
			if err := inv.printUnlisted(c.image, n.unlisted); err != nil {
				return err
			}
		}
	}
	return nil
}
