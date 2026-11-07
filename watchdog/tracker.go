package watchdog

import (
	"sort"
	"sync"
	"time"
)

// State is a target's liveness state.
type State string

const (
	Pending State = "pending" // expected, never reported, still inside its first timeout
	Up      State = "up"
	Down    State = "down"
)

// Rejection reasons returned by Tracker.Seen.
const (
	RejectMaxTargets = "max_targets"
	RejectInvalidKey = "invalid_key"
)

// Target is a snapshot of one tracked target.
type Target struct {
	Check    string
	Target   string
	State    State
	Since    time.Time
	LastSeen time.Time // zero: never reported
}

type target struct {
	state    State // "" until the first evaluation after a learned target's first message
	since    time.Time
	lastSeen time.Time
	// base is when the timeout window started: the last message, or the last
	// grace reset. It is separate from lastSeen so a grace reset never changes
	// the last_seen we report.
	base  time.Time
	dirty bool // changed since it was last written to KV
}

type check struct {
	loaded     bool // false: restored from KV, or invalid; not evaluated
	timeout    time.Duration
	maxTargets int
	targets    map[string]*target
}

// Tracker is the watchdog's pure logic: when was each target last seen, and
// what state is it in. It does no I/O; every method takes the current time.
type Tracker struct {
	mu     sync.Mutex
	checks map[string]*check
}

func NewTracker() *Tracker { return &Tracker{checks: map[string]*check{}} }

func (tr *Tracker) check(name string) *check {
	c := tr.checks[name]
	if c == nil {
		c = &check{targets: map[string]*target{}}
		tr.checks[name] = c
	}
	return c
}

// Restore adds a target read from the state bucket at startup. A down target
// stays down until it reports. Any other target gets a full timeout of grace.
func (tr *Tracker) Restore(t Target, now time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	nt := &target{state: t.State, since: t.Since, lastSeen: t.LastSeen, base: now}
	if t.State == Down {
		nt.base = time.Time{}
	}
	tr.check(t.Check).targets[t.Target] = nt
}

// SetCheck loads or replaces a check. Existing targets keep their state and
// get a fresh grace period. Expected targets that are not tracked yet start
// pending.
func (tr *Tracker) SetCheck(c Check, now time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	ch := tr.check(c.Name)
	ch.loaded = true
	ch.timeout = c.Timeout
	ch.maxTargets = c.MaxTargets
	for _, name := range c.Expected {
		if ch.targets[name] == nil {
			ch.targets[name] = &target{state: Pending, since: now, base: now, dirty: true}
		}
	}
	resetGrace(ch, now)
}

// UnloadCheck stops evaluating a check but keeps its targets. It is used when
// a check becomes invalid.
func (tr *Tracker) UnloadCheck(name string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if ch := tr.checks[name]; ch != nil {
		ch.loaded = false
	}
}

// RemoveCheck forgets a check and its targets.
func (tr *Tracker) RemoveCheck(name string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	delete(tr.checks, name)
}

// Forget drops one target. If it reports again it is learned again.
func (tr *Tracker) Forget(checkName, targetName string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if ch := tr.checks[checkName]; ch != nil {
		delete(ch.targets, targetName)
	}
}

// Seen records a message for a target. It returns a rejection reason, or ""
// when the message was recorded.
func (tr *Tracker) Seen(checkName, targetName string, now time.Time) string {
	if !ValidTarget(targetName) {
		return RejectInvalidKey
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	ch := tr.checks[checkName]
	if ch == nil || !ch.loaded {
		return "" // a message that raced the check's removal
	}
	t := ch.targets[targetName]
	if t == nil {
		if len(ch.targets) >= ch.maxTargets {
			return RejectMaxTargets
		}
		t = &target{}
		ch.targets[targetName] = t
	}
	t.lastSeen = now
	t.base = now
	return ""
}

// Evaluate applies state changes. It returns the targets that changed state
// now, and every target that needs writing to KV: those that changed now,
// new pending targets, and those whose earlier write failed.
func (tr *Tracker) Evaluate(now time.Time) (changed, dirty []Target) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for cname, ch := range tr.checks {
		if !ch.loaded {
			continue
		}
		for tname, t := range ch.targets {
			fresh := now.Sub(t.base) <= ch.timeout
			var to State
			switch {
			case fresh && !t.lastSeen.IsZero() && t.state != Up:
				to = Up
			case !fresh && t.state != Down:
				to = Down
			}
			if to != "" {
				t.state, t.since, t.dirty = to, now, true
				changed = append(changed, snapshot(cname, tname, t))
			}
			if t.dirty {
				dirty = append(dirty, snapshot(cname, tname, t))
			}
		}
	}
	sortTargets(changed)
	sortTargets(dirty)
	return changed, dirty
}

// Written clears a target's dirty flag after a successful KV write, unless it
// changed again in the meantime.
func (tr *Tracker) Written(t Target) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if ch := tr.checks[t.Check]; ch != nil {
		if cur := ch.targets[t.Target]; cur != nil && cur.state == t.State && cur.since.Equal(t.Since) {
			cur.dirty = false
		}
	}
}

// ResetGrace gives every target that is not down a full timeout before it
// can go down. It is called on reconnect: silence during a disconnect is
// ours, not the targets'.
func (tr *Tracker) ResetGrace(now time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, ch := range tr.checks {
		resetGrace(ch, now)
	}
}

func resetGrace(ch *check, now time.Time) {
	for _, t := range ch.targets {
		if t.state != Down {
			t.base = now
		}
	}
}

// Checks returns the names of every check the tracker holds, loaded or not.
func (tr *Tracker) Checks() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	names := make([]string, 0, len(tr.checks))
	for n := range tr.checks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Targets returns a snapshot of the targets matching the filters. An empty
// filter matches everything. Learned targets awaiting their first evaluation
// are left out.
func (tr *Tracker) Targets(checkName string, state State) []Target {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []Target
	for cname, ch := range tr.checks {
		if checkName != "" && cname != checkName {
			continue
		}
		for tname, t := range ch.targets {
			if t.state == "" || (state != "" && t.state != state) {
				continue
			}
			out = append(out, snapshot(cname, tname, t))
		}
	}
	sortTargets(out)
	return out
}

// Counts returns the number of targets per check per state.
func (tr *Tracker) Counts() map[string]map[State]int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := map[string]map[State]int{}
	for cname, ch := range tr.checks {
		m := map[State]int{Pending: 0, Up: 0, Down: 0}
		for _, t := range ch.targets {
			if t.state != "" {
				m[t.state]++
			}
		}
		out[cname] = m
	}
	return out
}

func snapshot(cname, tname string, t *target) Target {
	return Target{Check: cname, Target: tname, State: t.state, Since: t.since, LastSeen: t.lastSeen}
}

func sortTargets(ts []Target) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Check != ts[j].Check {
			return ts[i].Check < ts[j].Check
		}
		return ts[i].Target < ts[j].Target
	})
}
