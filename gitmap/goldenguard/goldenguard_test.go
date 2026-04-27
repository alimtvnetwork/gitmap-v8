package goldenguard

// Tests for the AllowUpdate dual gate. The function is tiny but its
// failure mode (silently allowing a CI fixture rewrite) is severe, so
// every branch is pinned: trigger-off, trigger-on+allow-on, and the
// two trigger-on+allow-bad cases that MUST fail loudly.

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestAllowUpdate_TriggerOff_IsFalse: when the per-test trigger is
// off the function must short-circuit to false WITHOUT consulting
// the env var. This is the hot path in CI — it must never touch
// os.Getenv-driven branches that could call t.Fatalf.
func TestAllowUpdate_TriggerOff_IsFalse(t *testing.T) {
	t.Setenv(AllowUpdateEnv, "1") // even with allow ON, trigger OFF wins
	if AllowUpdate(t, false) {
		t.Fatalf("AllowUpdate(false, allow=1) = true, want false "+
			"(trigger-off must short-circuit before reading %s)",
			AllowUpdateEnv)
	}
}

// TestAllowUpdate_BothOn_IsTrue: the only path that returns true.
// Documents the exact value pairing — trigger=true AND env=="1".
func TestAllowUpdate_BothOn_IsTrue(t *testing.T) {
	t.Setenv(AllowUpdateEnv, "1")
	if !AllowUpdate(t, true) {
		t.Fatalf("AllowUpdate(true, allow=1) = false, want true")
	}
}

// TestAllowUpdate_TriggerOnAllowMissing_Fails: the failure path that
// catches a stray -update flag or GITMAP_UPDATE_GOLDEN=1 in CI when
// the dedicated allow var was (correctly) NOT set. We use a sub-test
// so we can capture the t.Fatalf via a custom testing.TB harness.
func TestAllowUpdate_TriggerOnAllowMissing_Fails(t *testing.T) {
	t.Setenv(AllowUpdateEnv, "") // empty = not set (per Go env semantics)
	msg := captureFatal(t, func(tt testing.TB) {
		_ = AllowUpdate(tt, true)
	})
	if !strings.Contains(msg, AllowUpdateEnv) {
		t.Fatalf("fatal message missing env var name %q\n got: %s",
			AllowUpdateEnv, msg)
	}
	if !strings.Contains(msg, "double-gate") {
		t.Fatalf("fatal message should explain the double-gate "+
			"design so CI failure is actionable\n got: %s", msg)
	}
}

// TestAllowUpdate_TriggerOnAllowWrongValue_Fails: typo guard. The
// allow var is intentionally narrow (literal "1" only) so common
// misspellings ("true", "yes") fail closed instead of unlocking.
func TestAllowUpdate_TriggerOnAllowWrongValue_Fails(t *testing.T) {
	for _, bad := range []string{"true", "yes", "y", "TRUE", "0", " 1 "} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv(AllowUpdateEnv, bad)
			msg := captureFatal(t, func(tt testing.TB) {
				_ = AllowUpdate(tt, true)
			})
			if len(msg) == 0 {
				t.Fatalf("AllowUpdate accepted bogus allow=%q "+
					"(only literal \"1\" must unlock the gate)",
					bad)
			}
		})
	}
	// Belt-and-braces: the env var must be unset on test exit so
	// other tests in this package start from a known state. t.Setenv
	// already restores it, but Unsetenv after the loop documents
	// the intent for the human reader.
	_ = os.Unsetenv(AllowUpdateEnv)
}

// fatalRecorder wraps a testing.TB and intercepts Fatalf, recording
// the message instead of aborting the test process. It satisfies
// testing.TB by embedding the original TB value (which provides the
// unexported private() method required by the interface).
type fatalRecorder struct {
	testing.TB
	msg string
}

func (f *fatalRecorder) Helper() {
	f.TB.Helper()
}

// Fatalf records the formatted message and calls runtime.Goexit so
// that execution of the current goroutine stops (mirroring the real
// testing.T.Fatalf behaviour) without aborting the parent test.
func (f *fatalRecorder) Fatalf(format string, args ...interface{}) {
	if f.msg == "" {
		f.msg = fmt.Sprintf(format, args...)
	}
	runtime.Goexit()
}

// captureFatal runs fn against a fatalRecorder stub that records the
// first Fatalf call instead of aborting the goroutine. Returns the
// captured message (empty if fn never called Fatalf). Implemented
// as a goroutine + runtime.Goexit because testing.T.Fatalf calls
// runtime.Goexit which is goroutine-scoped — running fn in a child
// goroutine isolates the abort from the parent test runner.
func captureFatal(t testing.TB, fn func(testing.TB)) string {
	rec := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(rec)
	}()
	<-done
	return rec.msg
}
