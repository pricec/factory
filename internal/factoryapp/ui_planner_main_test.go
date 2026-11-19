package factoryapp

import (
	"strings"
	"testing"
)

// TestUIPlannerIsMainView pins the planner-first UI: the Planner chat is the
// default on desktop and phone, alerts and tasks live on their own tabs, and
// every chat send carries the device hint for the planner.
func TestUIPlannerIsMainView(t *testing.T) {
	ui := string(uiHTML)
	for name, wants := range map[string][]string{
		"desktop tabs": {
			`<button id="tab-planner" class="active">Planner</button>`, `id="tab-chat">Tasks</button>`, `id="tab-alerts"`,
			`let tab = "planner";`, `const CHAT_TABS = ["planner", "chat", "alerts"]`,
			`#inbox3.v-planner #alertpane, #inbox3.v-planner #taskpane`, // planner view hides alerts and tasks
			`#inbox3.v-alerts #centerpane`, `#inbox3.v-tasks #alertpane`,
			`st.selected = STANDING_ID`, // Planner tab shows the standing chat
		},
		"phone": {
			`{ name: h || "planner", arg: null }`, `if (!P_TAB[r.name]) r.name = "planner"`,
			`r.name === "task" && r.arg === STANDING_ID`, `history.replaceState(null, "", "#/planner")`,
			`data-tab="planner"`, `data-tab="alerts"`, `id="p-cnt-alerts"`,
			`function pRenderAlerts(`, `html.p-planner #p-composer`, `case "planner": case "task": pRenderThread()`,
			`P.r.name === "planner" ? STANDING_ID`,
		},
		"device hint": {
			`const client = isPhone() ? "mobile" : "desktop"`, `{ text, task_id: taskID, client } : { text, client }`, `postJ("/v1/tasks", { text, client })`,
		},
	} {
		for _, want := range wants {
			if !strings.Contains(ui, want) {
				t.Errorf("%s: UI missing %q", name, want)
			}
		}
	}
	// Alerts are no longer rendered inside the phone Tasks list.
	i := strings.Index(ui, "function pRenderInbox()")
	j := strings.Index(ui[i:], "function pRenderThread()")
	if i < 0 || j < 0 || strings.Contains(ui[i:i+j], "pAlertRow") {
		t.Error("phone Tasks list must not render alerts (they have their own tab)")
	}
	// Desktop alerts pane is unreachable from the Tasks view.
	if strings.Contains(ui, `data-tab="inbox"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11L2 12v6a2 2 0 002 2h16a2 2 0 002-2v-6l-3.45-6.89A2 2 0 0016.76 4H7.24a2 2 0 00-1.79 1.11z"/></svg><span>Inbox</span>`) {
		t.Error("phone Inbox tab should be labelled Tasks")
	}
}
