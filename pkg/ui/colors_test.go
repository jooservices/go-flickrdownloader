package ui

import "testing"

func TestEnableVirtualTerminalIsNoop(t *testing.T) {
	enableVirtualTerminal() // must not panic
}

func TestColorsEnabledRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorsEnabled() {
		t.Fatal("expected colors disabled when NO_COLOR is set")
	}
}

func TestColorsEnabledRespectsDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorsEnabled() {
		t.Fatal("expected colors disabled when TERM=dumb")
	}
}
