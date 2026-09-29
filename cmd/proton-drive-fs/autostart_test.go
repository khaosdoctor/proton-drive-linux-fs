package main

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	graphical := renderUnit("/opt/bin/pdfs", false)
	headless := renderUnit("/opt/bin/pdfs", true)

	for _, unit := range []string{graphical, headless} {
		if strings.Contains(unit, "@BINDIR@") || !strings.Contains(unit, "ExecStart=/opt/bin/pdfs mount -foreground") {
			t.Errorf("binary path not filled in:\n%s", unit)
		}
	}

	// PartOf would stop the mount whenever the desktop restarts graphical-session.target.
	if !strings.Contains(graphical, "WantedBy=graphical-session.target") || strings.Contains(graphical, "PartOf=") {
		t.Errorf("graphical unit should start with the graphical session without being stopped by it:\n%s", graphical)
	}

	if !strings.Contains(headless, "WantedBy=default.target") || strings.Contains(headless, "WantedBy=graphical-session.target") {
		t.Errorf("headless unit still tied to the graphical session:\n%s", headless)
	}
}
