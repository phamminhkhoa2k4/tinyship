package worker

import "testing"

func TestCrashPointMatchesOnlyTheNamedStep(t *testing.T) {
	t.Setenv("TINYSHIP_CRASH_AT", "health")
	if !crashPoint("health") {
		t.Error("crashPoint must match the step named by TINYSHIP_CRASH_AT")
	}
	if crashPoint("pull") {
		t.Error("crashPoint must not match a different step")
	}
	t.Setenv("TINYSHIP_CRASH_AT", "")
	if crashPoint("health") {
		t.Error("crashPoint must be inert when the variable is empty")
	}
}
