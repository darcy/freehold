package box

import (
	"fmt"
	"strconv"
	"strings"

	"freehold/platform/provisioning/drive"
	"freehold/platform/provisioning/planebase"
)

// This file is the plane-placement CHOICE: it turns the read-only storage
// inventory into a plain-language selection for a non-expert operator, rules
// out anything freehold must not touch, requires an explicit confirmation for
// risky choices, and reconnects to (or erases) freehold's own previous data.

// selectPlacement drives the choice from an inventory and returns the backend
// + thin pool the tenant LVs land in.
func (e *Engine) selectPlacement(inv planebase.Inventory) (*placement, error) {
	opts := planebase.BuildOptions(inv, e.F.RelayDomain)
	chosen, err := e.chooseBackend(opts)
	if err != nil {
		return nil, err
	}
	if e.mine(chosen) {
		if err := e.resolveFreeholdData(chosen); err != nil {
			return nil, err
		}
	}
	switch chosen.Kind {
	case planebase.KindReuseZpool:
		return &placement{pool: chosen.Backend, kind: planebase.KindZfs}, nil
	case planebase.KindReuseVG:
		p, err := e.choosePool(chosen)
		if err != nil {
			return nil, err
		}
		p.kind = planebase.KindLvmThin
		return p, nil
	default:
		return nil, fmt.Errorf("creating a new storage backend is a later phase — choose an existing backend, or clean a spare disk and re-run")
	}
}

// mine reports whether an option carries THIS world's freehold plane (its
// provenance domain matches e.F.RelayDomain). Another world's data — or a
// name-only pool — is ordinary reuse, never a reconnect.
func (e *Engine) mine(opt planebase.Option) bool {
	matched, hasDomains := opt.Freehold.DomainMatches(e.F.RelayDomain)
	return matched && hasDomains
}

// chooseBackend picks a backend from the classified options: an explicit
// --plane-pool, the single usable option (with a one-keystroke confirm), or an
// interactive menu. It NEVER guesses when several are usable — --yes refuses
// and names --plane-pool.
func (e *Engine) chooseBackend(opts []planebase.Option) (planebase.Option, error) {
	if e.F.PlanePool != "" {
		opt, ok := planebase.FindOption(opts, e.F.PlanePool)
		if !ok {
			return planebase.Option{}, fmt.Errorf("no storage backend named %q on this host:\n%s", e.F.PlanePool, describeOptions(opts))
		}
		if opt.Kind == planebase.KindBlocked {
			return planebase.Option{}, fmt.Errorf("storage %q cannot be used: %s", e.F.PlanePool, opt.Reason)
		}
		if opt.Kind == planebase.KindCreateDevice {
			return planebase.Option{}, fmt.Errorf("preparing a new storage backend on %s is a later phase — choose an existing backend", opt.Device)
		}
		return opt, nil
	}

	usable := usableOptions(opts)
	if len(usable) == 0 {
		msg := "no usable storage found on this host — freehold never erases a disk that carries data"
		if len(deferredOptions(opts)) > 0 {
			msg += " (preparing a new disk is a later phase)"
		}
		return planebase.Option{}, fmt.Errorf("%s:\n%s", msg, describeOptions(opts))
	}
	if len(usable) == 1 {
		opt := usable[0]
		// The one-keystroke confirm for the common safe case; THIS world's own
		// data is confirmed by the keep/erase step instead.
		if !e.mine(opt) && opt.Safety == planebase.Safe {
			if err := e.confirmYes("Freehold will store its data in "+opt.Title, opt.Impact); err != nil {
				return planebase.Option{}, err
			}
		}
		return opt, nil
	}
	if e.F.Yes {
		return planebase.Option{}, fmt.Errorf("several storage backends found; --yes will not guess — pick one with --plane-pool:\n%s", describeOptions(opts))
	}
	return e.promptBackend(opts, usable)
}

// promptBackend renders the plain-language menu. Ruled-out backends are shown
// with their reason so the operator understands why, but cannot be chosen.
func (e *Engine) promptBackend(opts, usable []planebase.Option) (planebase.Option, error) {
	rec := planebase.Recommend(opts)
	fmt.Fprint(e.Out, "\n  How should freehold store your data?\n\n")
	def := 0
	for i, o := range usable {
		mark := ""
		if rec >= 0 && sameOption(o, opts[rec]) {
			mark = "   ← recommended"
			def = i + 1
		}
		fmt.Fprintf(e.Out, "   %d) %s%s\n      %s\n", i+1, o.Title, mark, o.Impact)
	}
	if blocked := blockedOptions(opts); len(blocked) > 0 {
		fmt.Fprint(e.Out, "\n  Freehold will NOT touch:\n")
		for _, o := range blocked {
			fmt.Fprintf(e.Out, "   - %s: %s\n", o.Title, o.Reason)
		}
	}
	if deferred := deferredOptions(opts); len(deferred) > 0 {
		fmt.Fprint(e.Out, "\n  Seen, but freehold cannot prepare new disks yet:\n")
		for _, o := range deferred {
			fmt.Fprintf(e.Out, "   - %s\n", o.Title)
		}
	}
	fmt.Fprint(e.Out, "\n   x) stop — make no changes\n\n")
	hint := ""
	if def > 0 {
		hint = fmt.Sprintf(", default %d", def)
	}
	answer, err := e.Prompt(fmt.Sprintf("choose [1-%d%s / x]", len(usable), hint))
	if err != nil {
		return planebase.Option{}, err
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer == "x" || answer == "q" || answer == "stop" {
		return planebase.Option{}, fmt.Errorf("stopped — no changes made")
	}
	if answer == "" && def > 0 {
		answer = strconv.Itoa(def)
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(usable) {
		return planebase.Option{}, fmt.Errorf("please choose a number between 1 and %d (or x to stop)", len(usable))
	}
	chosen := usable[n-1]
	return chosen, nil
}

// choosePool resolves the thin pool within a VG: the --thin-pool flag
// (adopt-if-present, else carve), --yes (reuse the pool freehold's data lives
// in, else the first detected pool), or the interactive reuse/carve prompt.
func (e *Engine) choosePool(opt planebase.Option) (*placement, error) {
	pools := planebase.PoolNames(opt)
	has := func(name string) bool {
		for _, p := range pools {
			if p == name {
				return true
			}
		}
		return false
	}

	if e.F.ThinPool != "" {
		if !planebase.ValidStorageName(e.F.ThinPool) {
			return nil, fmt.Errorf("thin-pool name %q is not allowed (use letters, digits, '.', '_', '-')", e.F.ThinPool)
		}
		// A reconnect was promised (resolveFreeholdData kept the data): an
		// explicit --thin-pool must not quietly place this world in a DIFFERENT
		// pool and orphan the previous plane.
		if e.mine(opt) {
			if _, holders := firstPool(opt, e.F.RelayDomain); len(holders) > 0 && !strIn(holders, e.F.ThinPool) {
				return nil, fmt.Errorf("previous freehold data lives in pool %q — refusing to place this world in %q and orphan it; pass --thin-pool %s, or erase the previous data first (--erase-freehold)", strings.Join(holders, ", "), e.F.ThinPool, holders[0])
			}
		}
		if has(e.F.ThinPool) {
			if err := e.confirmPoolShare(opt, e.F.ThinPool); err != nil {
				return nil, err
			}
			return &placement{pool: opt.Backend, thinPool: e.F.ThinPool, created: false}, nil
		}
		return &placement{pool: opt.Backend, thinPool: e.F.ThinPool, created: true}, nil
	}
	if len(pools) == 0 {
		// The VG has no thin pool yet: carve the default.
		return &placement{pool: opt.Backend, thinPool: drive.FreshThinPool, created: true}, nil
	}

	// On a freehold reconnect, prefer the pool that actually carries the
	// freehold data — pools are unordered, and picking the wrong one would
	// orphan the previous plane. Several holders is ambiguous: name them and
	// require an explicit choice (never default to an empty name).
	first, fhHolders := firstPool(opt, e.F.RelayDomain)
	if e.mine(opt) && len(fhHolders) > 1 {
		if e.F.Yes {
			return nil, fmt.Errorf("several pools under “%s” hold freehold data (%s); --yes will not guess — pass --thin-pool", opt.Backend, strings.Join(fhHolders, ", "))
		}
		fmt.Fprintf(e.Out, "\n  Several storage pools under “%s” hold freehold data: %s\n", opt.Backend, strings.Join(fhHolders, ", "))
		answer, err := e.Prompt("type the pool to reconnect to")
		if err != nil {
			return nil, err
		}
		answer = strings.TrimSpace(answer)
		if !has(answer) {
			return nil, fmt.Errorf("%q is not one of this storage's pools (%s)", answer, strings.Join(pools, ", "))
		}
		if err := e.confirmPoolShare(opt, answer); err != nil {
			return nil, err
		}
		return &placement{pool: opt.Backend, thinPool: answer, created: false}, nil
	}

	if e.F.Yes {
		if err := e.confirmPoolShare(opt, first); err != nil {
			return nil, err
		}
		return &placement{pool: opt.Backend, thinPool: first, created: false}, nil
	}

	fmt.Fprintf(e.Out, "\n  Storage “%s” already holds: %s\n", opt.Backend, poolSummary(opt.Pools))
	fmt.Fprintf(e.Out, "    r      reuse %q\n", first)
	fmt.Fprintf(e.Out, "    <name> create a NEW pool of that name (%d GB)\n", e.F.PoolSizeGB)
	answer, err := e.Prompt("pool choice [r = reuse / type a new pool name]")
	if err != nil {
		return nil, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || answer == "r" || answer == "R" || answer == first {
		if err := e.confirmPoolShare(opt, first); err != nil {
			return nil, err
		}
		return &placement{pool: opt.Backend, thinPool: first, created: false}, nil
	}
	if has(answer) {
		if err := e.confirmPoolShare(opt, answer); err != nil {
			return nil, err
		}
		return &placement{pool: opt.Backend, thinPool: answer, created: false}, nil
	}
	if !planebase.ValidStorageName(answer) {
		return nil, fmt.Errorf("thin-pool name %q is not allowed (use letters, digits, '.', '_', '-')", answer)
	}
	sizeAnswer, err := e.Prompt(fmt.Sprintf("new pool %q size GB (blank = %d)", answer, e.F.PoolSizeGB))
	if err != nil {
		return nil, err
	}
	size, err := parseGB(sizeAnswer, e.F.PoolSizeGB, "new thin-pool size GB")
	if err != nil {
		return nil, err
	}
	e.F.PoolSizeGB = size
	return &placement{pool: opt.Backend, thinPool: answer, created: true}, nil
}

// firstPool returns the pool to reuse by default (the sole pool carrying
// THIS world's freehold data when there is exactly one, else the first pool)
// and the list of pools that carry this world's freehold data. Another world's
// freehold pools are not holders — they are ordinary reuse.
func firstPool(opt planebase.Option, domain string) (first string, fhHolders []string) {
	for _, p := range opt.Pools {
		if matched, has := p.Freehold.DomainMatches(domain); matched && has {
			fhHolders = append(fhHolders, p.Name)
		}
	}
	if len(opt.Pools) > 0 {
		first = opt.Pools[0].Name
	}
	if len(fhHolders) == 1 {
		first = fhHolders[0]
	}
	return first, fhHolders
}

// resolveFreeholdData handles a backend that carries THIS world's previous
// data: keep and reconnect (requires the SAME relay domain, since volume names
// are domain-derived), or erase it (destroying ONLY this world's entries —
// another world's freehold data on the same backend is never touched) and
// start fresh.
func (e *Engine) resolveFreeholdData(opt planebase.Option) error {
	domains := opt.Freehold.Domains
	// A pool can be recognized as freehold's from its NAME alone (an empty or
	// dedicated pool with no tenant entries yet): there is nothing to
	// reconnect to or erase, so there is no decision to make.
	if len(domains) == 0 {
		return nil
	}
	want, _ := planebase.NormalizeDomain(e.F.RelayDomain)
	matched := ""
	for _, d := range domains {
		if d == want {
			matched = d
		}
	}
	// Defensive: selectPlacement routes only this world's plane here, so a
	// match is expected. A foreign-only backend is ordinary reuse, not ours.
	if matched == "" {
		return nil
	}
	var foreign []string
	for _, d := range domains {
		if d != matched {
			foreign = append(foreign, d)
		}
	}

	erase := e.F.EraseFreehold
	if !erase && !e.F.Yes {
		fmt.Fprintf(e.Out, "\n  We found your previous freehold data (for %q) on %s.\n", matched, opt.Backend)
		if len(foreign) > 0 {
			fmt.Fprintf(e.Out, "  %s also holds freehold data for another world (%s) — freehold will not touch it.\n", opt.Backend, strings.Join(foreign, ", "))
		}
		fmt.Fprint(e.Out, "    k) keep it and reconnect\n    e) erase it and start fresh\n")
		answer, err := e.Prompt("choose [k]")
		if err != nil {
			return err
		}
		a := strings.TrimSpace(strings.ToLower(answer))
		erase = a == "e" || a == "erase"
	}

	if !erase {
		fmt.Fprintf(e.Out, "  reconnecting to your previous freehold data for %q\n", matched)
		return nil
	}

	kind := "lvmth"
	if opt.Kind == planebase.KindReuseZpool {
		kind = "zfs"
	}
	// Erase ONLY this world's domain. Other worlds on this backend keep theirs.
	for _, tenant := range []string{"relay", "cp", "k3s-volumes"} {
		ok, out := e.RunBin(e.Bins.Self, []string{
			"storage", "destroy",
			"--addr", e.F.Addr, "--agent-dir", OpsDir(), "--target", e.F.Target,
			"--tenant", tenant, "--domain", matched, "--pool", opt.Backend, "--kind", kind,
		})
		if !ok {
			return fmt.Errorf("erasing previous freehold data (%s/%s) failed:\n%s", matched, tenant, out)
		}
	}
	fmt.Fprintf(e.Out, "  erased previous freehold data for %s\n", matched)
	return nil
}

// requireShare is the share-word gate: used when a choice would share storage
// with data already in use. Safe options never reach it.
func (e *Engine) requireShare(title, impact string) error {
	if e.F.ConfirmSharedPool {
		return nil
	}
	if e.F.Yes {
		return fmt.Errorf("%s\n  this shares storage with data already in use — re-run with --confirm-shared-pool if you are sure", impact)
	}
	fmt.Fprintf(e.Out, "\n  %s\n  %s\n", title, impact)
	answer, err := e.Prompt(`type "share" to proceed (anything else stops)`)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(answer), "share") {
		return nil
	}
	return fmt.Errorf("stopped — no changes made")
}

// confirmPoolShare requires `share` when the chosen thin pool already holds
// OTHER volumes (live guest disks or another world's freehold LVs sharing its
// capacity). THIS world's own LVs riding the pool are not "sharing" —
// reconnecting to them is the point.
func (e *Engine) confirmPoolShare(opt planebase.Option, pool string) error {
	riders := 0
	for _, p := range opt.Pools {
		if p.Name == pool {
			riders = p.OtherVolumes
		}
	}
	if riders == 0 {
		return nil
	}
	return e.requireShare(
		fmt.Sprintf("“%s” storage already holds %d of your existing volumes", pool, riders),
		"Freehold would share this space with them, and they could run out of room.",
	)
}

// confirmYes is the one-keystroke confirm for the common safe single-answer
// case; --yes skips it.
func (e *Engine) confirmYes(what, impact string) error {
	if e.F.Yes {
		return nil
	}
	fmt.Fprintf(e.Out, "\n  %s\n  %s\n", what, impact)
	answer, err := e.Prompt("continue? [Y/n]")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return nil
	}
	return fmt.Errorf("stopped — no changes made")
}

// ---- small render helpers ---------------------------------------------------

// usableOptions are the backends this phase can actually drive: reuse of an
// existing zpool or VG. Device-create options are DEFERRED (returned by
// deferredOptions for display only), so they never appear as selectable.
func usableOptions(opts []planebase.Option) []planebase.Option {
	var out []planebase.Option
	for _, o := range opts {
		if o.Kind == planebase.KindReuseZpool || o.Kind == planebase.KindReuseVG {
			out = append(out, o)
		}
	}
	return out
}

func blockedOptions(opts []planebase.Option) []planebase.Option {
	var out []planebase.Option
	for _, o := range opts {
		if o.Kind == planebase.KindBlocked {
			out = append(out, o)
		}
	}
	return out
}

// deferredOptions are recognized but not yet actionable (preparing a new
// backend on a clean disk is a later phase) — shown so the operator knows the
// disk was seen and why it cannot be used yet.
func deferredOptions(opts []planebase.Option) []planebase.Option {
	var out []planebase.Option
	for _, o := range opts {
		if o.Kind == planebase.KindCreateDevice {
			out = append(out, o)
		}
	}
	return out
}

func sameOption(a, b planebase.Option) bool {
	return a.Backend == b.Backend && a.Device == b.Device
}

func strIn(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func describeOptions(opts []planebase.Option) string {
	var b strings.Builder
	for _, o := range opts {
		if o.Kind == planebase.KindBlocked {
			fmt.Fprintf(&b, "  - %s: cannot be used — %s\n", o.Title, o.Reason)
			continue
		}
		if o.Kind == planebase.KindCreateDevice {
			fmt.Fprintf(&b, "  - %s: preparing a new disk is a later phase\n", o.Title)
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", o.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

func poolSummary(pools []planebase.PoolInfo) string {
	var parts []string
	for _, p := range pools {
		s := fmt.Sprintf("%q (%.0f%% used", p.Name, p.DataPercent)
		if n := p.OtherVolumes; n > 0 {
			s += fmt.Sprintf(", %d volumes", n)
		}
		parts = append(parts, s+")")
	}
	return strings.Join(parts, ", ")
}
