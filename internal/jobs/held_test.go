package jobs

import "testing"

// TestHeldStateEdges: held (S3 dispatch gate) is non-terminal, releases
// only to pending (never straight to dispatched/running), and can be
// cancelled or superseded like any other non-terminal job.
func TestHeldStateEdges(t *testing.T) {
	if Terminal(StateHeld) {
		t.Fatal("held must be non-terminal")
	}
	for to, want := range map[string]bool{
		StatePending: true, StateCancelled: true, StateSuperseded: true,
		StateDispatched: false, StateRunning: false, StateDone: false, StateFailed: false,
	} {
		if legal[StateHeld][to] != want {
			t.Errorf("held -> %s: legal=%v want %v", to, legal[StateHeld][to], want)
		}
	}
	for from := range legal {
		if from != StateHeld && legal[from][StateHeld] {
			t.Errorf("%s -> held must be illegal (only creation parks a job)", from)
		}
	}
}
