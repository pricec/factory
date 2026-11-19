package factoryapp

import (
	"strings"
	"testing"
)

// TestUIPlannerEnterStaysOnPlanner: the focus-mode triage queue (default mode)
// must not move the selection when sending from the Planner tab.
func TestUIPlannerEnterStaysOnPlanner(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`function focusTriage() { return st.mode === "focus" && tab === "chat"; }`,
		`if (st.tasksAvailable && focusTriage()) {`,
		`applyQueue(fresh, focusTriage())`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("UI missing %q", want)
		}
	}
	// No remaining path advances the desktop selection by mode alone.
	i := strings.Index(ui, `$("chatform").onsubmit`)
	j := strings.Index(ui[i:], "Composer keys.")
	if i < 0 || j < 0 || strings.Contains(ui[i:i+j], `st.mode === "focus"`) {
		t.Error("chat submit must gate focus advance on focusTriage(), not st.mode alone")
	}
}

// TestUIDesktopPageLayout: secrets live in Settings, usage stats on the
// Observability page, and the Planner page is a one-viewport layout.
func TestUIDesktopPageLayout(t *testing.T) {
	ui := string(uiHTML)
	idx := func(s string) int { return strings.Index(ui, s) }
	in := func(inner, outerStart, outerEnd string) bool {
		a, b, c := idx(outerStart), idx(outerEnd), idx(inner)
		return a >= 0 && b > a && c > a && c < b
	}
	if !in(`id="credpanel"`, `<div id="settingsview"`, `<div id="phone">`) {
		t.Error("credentials panel must live inside #settingsview")
	}
	if !in(`id="usagepanel"`, `<div id="observabilityview"`, `<div id="settingsview"`) {
		t.Error("usage panel must live inside #observabilityview")
	}
	if idx(`id="usagepanel"`) < idx(`<main>`) {
		t.Error("usage panel must not sit above <main> on every page")
	}
	for _, want := range []string{
		`<button id="tab-observability">Observability</button>`, `id="obs-widgets"`, "OBS_WIDGETS", `observability: renderObservability`,
		// planner strip: shared usage chip + stats
		`id="pl-rings"`, `["l", $("pl-rings")]`, `id="plstrip"`, "function renderPlannerStrip()", `id="pl-waiting"`, `id="pl-alerts"`,
		// one viewport, only the chat log scrolls
		"body.t-planner { height: 100vh; height: 100dvh; overflow: hidden;", "body.t-planner #inbox3 { flex: 1; min-height: 0;",
		`classList.toggle("t-planner"`, "#chatlog { flex: 1; overflow-y: auto;",
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("UI missing %q", want)
		}
	}
	// phone layout still hides the desktop shell
	if !strings.Contains(ui, "html.phone body > header, html.phone #usagepanel, html.phone #credpanel, html.phone body > main { display: none !important; }") {
		t.Error("phone must keep hiding the desktop shell")
	}
}
