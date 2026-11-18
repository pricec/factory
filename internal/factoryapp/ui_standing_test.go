package factoryapp

import (
	"strings"
	"testing"
)

// TestUIStandingChatWiring pins the pinned "Planner" entry on both layouts:
// it opens the standing thread through the normal task routes (so the
// composer, SSE refresh and phone thread view all work unchanged), has no
// rename or close affordance, and is never part of the fetched task list.
func TestUIStandingChatWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`const STANDING_ID = "task_standing"`,
		`function standingRowHTML()`,
		`box.innerHTML = pin + st.tasks.map(`, // desktop: pinned above the list
		`data-go="task/' + STANDING_ID + '"`,  // phone: pinned row opens the task route
		`data-testid="standing-row"`,
		`t.standing ? "Planner" : esc(t.title)`,                  // phone title
		`t.standing ? "Ask the planner…"`,                        // phone composer
		`t.standing ? "ask the planner, or tell it what to do…"`, // desktop composer
		`t.status === "open" && !t.standing ? '<button class="p-btn" data-act="close"`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("UI missing %q", want)
		}
	}
	if strings.Contains(ui, `"/v1/tasks?status=open&include_standing`) {
		t.Error("the standing chat must not be fetched with the task list")
	}
}
