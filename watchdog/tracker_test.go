package watchdog

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func testCheck(expected ...string) Check {
	return Check{Name: "gw", Subject: "site.*.hb", TargetToken: 1, Timeout: 60 * time.Second, Expected: expected, MaxTargets: 3}
}

// states returns target -> state for the "gw" check.
func states(tr *Tracker) map[string]State {
	out := map[string]State{}
	for _, t := range tr.Targets("gw", "") {
		out[t.Target] = t.State
	}
	return out
}

func eval(tr *Tracker, now time.Time) []Target {
	changed, dirty := tr.Evaluate(now)
	for _, t := range dirty {
		tr.Written(t)
	}
	return changed
}

func TestLearnedTargetGoesUpThenDown(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(1))

	if got := eval(tr, at(10)); len(got) != 1 || got[0].State != Up {
		t.Fatalf("want a up, got %+v", got)
	}
	if got := eval(tr, at(61)); len(got) != 0 {
		t.Fatalf("still inside timeout, got %+v", got)
	}
	got := eval(tr, at(62))
	if len(got) != 1 || got[0].State != Down || !got[0].LastSeen.Equal(at(1)) {
		t.Fatalf("want a down with last_seen at(1), got %+v", got)
	}
	tr.Seen("gw", "a", at(100))
	if got := eval(tr, at(105)); len(got) != 1 || got[0].State != Up {
		t.Fatalf("want a back up, got %+v", got)
	}
}

func TestExpectedTargetNeverReports(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck("a"), at(0))

	changed, dirty := tr.Evaluate(at(1))
	if len(changed) != 0 || len(dirty) != 1 || dirty[0].State != Pending {
		t.Fatalf("want one dirty pending target and no change, got %+v %+v", changed, dirty)
	}
	tr.Written(dirty[0])

	got := eval(tr, at(61))
	if len(got) != 1 || got[0].State != Down || !got[0].LastSeen.IsZero() {
		t.Fatalf("want a down, never seen, got %+v", got)
	}
}

func TestDirtyUntilWritten(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(1))

	_, dirty := tr.Evaluate(at(2)) // write "fails": Written not called
	if len(dirty) != 1 {
		t.Fatalf("want 1 dirty, got %d", len(dirty))
	}
	changed, dirty := tr.Evaluate(at(3))
	if len(changed) != 0 || len(dirty) != 1 {
		t.Fatalf("want retry with no new change, got %+v %+v", changed, dirty)
	}
	tr.Written(dirty[0])
	if _, dirty := tr.Evaluate(at(4)); len(dirty) != 0 {
		t.Fatalf("want clean, got %+v", dirty)
	}
}

func TestWrittenIgnoresStaleSnapshot(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(1))
	_, first := tr.Evaluate(at(2)) // up
	eval(tr, at(70))               // down, written
	tr.Seen("gw", "a", at(71))
	tr.Evaluate(at(72)) // up again, not written
	tr.Written(first[0])
	if _, dirty := tr.Evaluate(at(73)); len(dirty) != 1 {
		t.Fatal("an old snapshot must not clear a newer change")
	}
}

func TestMaxTargetsAndInvalidKey(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	for _, n := range []string{"a", "b", "c"} {
		if r := tr.Seen("gw", n, at(1)); r != "" {
			t.Fatalf("%s rejected: %s", n, r)
		}
	}
	if r := tr.Seen("gw", "d", at(1)); r != RejectMaxTargets {
		t.Fatalf("want max_targets, got %q", r)
	}
	if r := tr.Seen("gw", "a", at(2)); r != "" {
		t.Fatalf("a known target is never rejected, got %q", r)
	}
	if r := tr.Seen("gw", "bad:key", at(2)); r != RejectInvalidKey {
		t.Fatalf("want invalid_key, got %q", r)
	}
}

func TestRestoreAfterRestart(t *testing.T) {
	tr := NewTracker()
	tr.Restore(Target{Check: "gw", Target: "up1", State: Up, Since: at(-500), LastSeen: at(-400)}, at(0))
	tr.Restore(Target{Check: "gw", Target: "down1", State: Down, Since: at(-300), LastSeen: at(-900)}, at(0))
	tr.Restore(Target{Check: "gw", Target: "pend1", State: Pending, Since: at(-10)}, at(0))
	tr.SetCheck(testCheck(), at(0))

	if got := eval(tr, at(30)); len(got) != 0 {
		t.Fatalf("restart must not cause changes inside the grace period, got %+v", got)
	}
	got := eval(tr, at(61))
	if len(got) != 2 || got[0].Target != "pend1" || got[1].Target != "up1" {
		t.Fatalf("want pend1 and up1 down after grace, got %+v", got)
	}
	if s := states(tr); s["down1"] != Down {
		t.Fatalf("down1 must stay down, got %v", s)
	}
}

func TestResetGraceAfterDisconnect(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(0))
	tr.Seen("gw", "b", at(0))
	eval(tr, at(1))
	eval(tr, at(200)) // both down
	tr.Seen("gw", "a", at(201))
	eval(tr, at(202)) // a up, b down

	// Disconnected from 210 to 400: nothing evaluated, nothing seen.
	tr.ResetGrace(at(400))
	if got := eval(tr, at(450)); len(got) != 0 {
		t.Fatalf("want no changes inside grace, got %+v", got)
	}
	got := eval(tr, at(461))
	if len(got) != 1 || got[0].Target != "a" || got[0].State != Down {
		t.Fatalf("want a down after grace, got %+v", got)
	}
	if !got[0].LastSeen.Equal(at(201)) {
		t.Fatalf("grace must not change last_seen, got %v", got[0].LastSeen)
	}
}

func TestUnloadedCheckIsNotEvaluated(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(0))
	eval(tr, at(1))
	tr.UnloadCheck("gw")
	if got := eval(tr, at(500)); len(got) != 0 {
		t.Fatalf("unloaded check evaluated: %+v", got)
	}
	if r := tr.Seen("gw", "b", at(500)); r != "" || len(tr.Targets("gw", "")) != 1 {
		t.Fatal("unloaded check must ignore messages")
	}
}

func TestForgetAndRemove(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck("a", "b"), at(0))
	tr.Forget("gw", "a")
	if s := states(tr); len(s) != 1 || s["b"] != Pending {
		t.Fatalf("got %v", s)
	}
	tr.RemoveCheck("gw")
	if len(tr.Checks()) != 0 {
		t.Fatal("check not removed")
	}
}

func TestEditKeepsStateAndResetsGrace(t *testing.T) {
	tr := NewTracker()
	tr.SetCheck(testCheck(), at(0))
	tr.Seen("gw", "a", at(0))
	eval(tr, at(1))
	tr.SetCheck(testCheck(), at(50)) // edited
	if got := eval(tr, at(100)); len(got) != 0 {
		t.Fatalf("edit must reset grace, got %+v", got)
	}
	if got := eval(tr, at(111)); len(got) != 1 || got[0].State != Down {
		t.Fatalf("want a down after the new grace, got %+v", got)
	}
}
