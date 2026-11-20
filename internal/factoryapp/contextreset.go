package factoryapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// contextClearedText is the visible note marking where the cutoff is.
const contextClearedText = "Planner context cleared"

// isResetCommand reports whether chat text is the "/reset" command.
func isResetCommand(text string) bool {
	return strings.EqualFold(strings.TrimSpace(text), "/reset")
}

// afterCutoff drops the thread rows at or before the cutoff message. A
// cutoff outside the fetched window is older than every row, so all stay.
func afterCutoff(thread []wire.MessageEnvelope, cutoff string) []wire.MessageEnvelope {
	if cutoff == "" {
		return thread
	}
	for i, e := range thread {
		if e.ID == cutoff {
			return thread[i+1:]
		}
	}
	return thread
}

// resetTaskContext wipes what the planner sees of a thread: it posts the
// visible note (brain.reply: no wake), then stores the note as the cutoff
// and, for the standing chat, clears the maintained summary. Nothing is
// deleted and the priority criteria are kept.
func (a *App) resetTaskContext(r *http.Request, taskID string) (wire.MessageEnvelope, error) {
	ctx := r.Context()
	data, _ := json.Marshal(map[string]string{"text": contextClearedText})
	note, err := a.postToTask(ctx, taskID, wire.MessageEnvelope{Type: tasks.TypeBrainReply, From: "factory", Payload: data})
	if err != nil {
		return note, err
	}
	return note, a.tasks.ResetContext(ctx, taskID, note.ID)
}

// handleResetContext serves POST /v1/tasks/{id}/reset_context -> {"message":note}.
func (a *App) handleResetContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.tasks.GetTask(r.Context(), id); errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+id)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	note, err := a.resetTaskContext(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": note})
}
