package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIPhoneFocusWiring pins the phone focus mode: it is the default inbox
// view, reuses the desktop queue helpers (no third copy of the queue/advance
// logic), has the counter/controls/empty state, and leaves the desktop alone.
func TestUIPhoneFocusWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		// phone state: focus is the default on every load; list is the switch
		`mode: "focus",          // inbox view: "focus" (default on every load) | "list"`,
		`id="p-fbar"`, `id="p-fcount"`, `data-act="view-focus"`, `data-act="view-list"`,
		`id="p-fnav"`, `data-act="focus-prev"`, `data-act="focus-skip"`, `data-act="focus-next"`,
		"Nothing needs your input", `id="p-fempty"`,
		"html.p-fthread", "html.p-inbox #p-fbar",
		// shared queue logic, used by both layouts
		"function applyQueue(", "function replyAdvance(", "function queueStep(", "function queueSkip(", "function fetchQueue(",
		"await pLoadFocus()", "await replyAdvance(id)", "pFocusGo(queueStep(1))", "pFocusGo(queueSkip())",
		"env(safe-area-inset-bottom", // existing safe-area vars are reused
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("UI missing %q", want)
		}
	}
	// the queue/advance logic exists exactly once (shared, not forked).
	for _, once := range []string{"function mergeQueue(", "function advance(", "needs_operator=true&limit=500"} {
		if n := strings.Count(ui, once); n != 1 {
			t.Errorf("%q appears %d times, want 1 (queue logic must be shared)", once, n)
		}
	}
	// desktop focus mode is still wired to the shared helpers.
	for _, want := range []string{
		`$("focus-next").onclick = () => focusStep(1);`, `$("modetoggle").onclick`,
		"await replyAdvance(replied);", "applyQueue(fresh, st.mode === \"focus\")",
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("desktop focus wiring missing %q", want)
		}
	}
	// no swipe-to-navigate in focus mode: it would fight vertical scrolling and the row swipe.
	if strings.Contains(ui, "pFocusSwipe") {
		t.Error("focus mode must not add swipe navigation")
	}
}

// TestUIFocusQueueJS runs the shared queue logic (mergeQueue/advance/
// applyQueue/queueStep/queueSkip/replyAdvance) under node. Skipped when node
// is absent, like TestUIUsageChipJS.
func TestUIFocusQueueJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	ui := string(uiHTML)
	a := strings.Index(ui, "// mergeQueue keeps the on-screen queue stable")
	endMark := "window.__queue.applyQueue = applyQueue;"
	b := strings.Index(ui, endMark)
	if a < 0 || b < a {
		t.Fatal("queue block not found")
	}
	const pre = `
globalThis.window = {};
const st = { queue: [], focusId: null, selected: null, thread: null, tasksAvailable: true };
const api404 = (e) => e && e.status === 404;
let waiting = [];
const getJ = async () => ({ tasks: waiting.map((id) => ({ id })) });
`
	const tests = `
(async () => {
const assert = require("assert");
// mergeQueue: stable order, answered drop out, new arrivals go to the end.
assert.deepStrictEqual(mergeQueue(["a", "b", "c"], ["c", "x", "a"]), ["a", "c", "x"]);
assert.deepStrictEqual(mergeQueue([], ["p", "q"]), ["p", "q"]);
assert.deepStrictEqual(mergeQueue(["a"], []), []);
// advance: next with wrap; head when current is gone; "" when alone/empty.
assert.strictEqual(advance(["a", "b", "c"], "a"), "b");
assert.strictEqual(advance(["a", "b", "c"], "c"), "a");
assert.strictEqual(advance(["a", "b"], "gone"), "a");
assert.strictEqual(advance(["a"], "a"), "");
assert.strictEqual(advance([], "a"), "");
// applyQueue: focuses the head initially; keeps focus while still waiting.
applyQueue(["a", "b", "c"], true);
assert.strictEqual(st.focusId, "a");
applyQueue(["a", "b", "c", "n"], true);       // a new waiting task queues at the end, focus stays
assert.deepStrictEqual(st.queue, ["a", "b", "c", "n"]);
assert.strictEqual(st.focusId, "a");
applyQueue(["b", "c", "n"], true);            // focused one answered elsewhere: head
assert.strictEqual(st.focusId, "b");
applyQueue([], true);                         // nothing waiting: empty state
assert.strictEqual(st.focusId, null);
// queueStep / queueSkip
st.queue = ["a", "b", "c"]; st.focusId = "a";
assert.strictEqual(queueStep(1), "b");
assert.strictEqual(queueStep(-1), "c");
st.focusId = "c"; assert.strictEqual(queueStep(1), "a");
st.queue = []; assert.strictEqual(queueStep(1), "");
st.queue = ["a"]; st.focusId = "a"; assert.strictEqual(queueSkip(), "");
assert.deepStrictEqual(st.queue, ["a"]);
st.queue = ["a", "b", "c"]; st.focusId = "a";
assert.strictEqual(queueSkip(), "b");         // skip sends it to the back
assert.deepStrictEqual(st.queue, ["b", "c", "a"]);
// replyAdvance: answering "b" moves to "c"; the server no longer lists "b";
// a task that started waiting meanwhile joins at the end, not the screen.
st.queue = ["a", "b", "c"]; st.focusId = "b";
waiting = ["a", "c", "late"];
await replyAdvance("b");
assert.strictEqual(st.focusId, "c");
assert.deepStrictEqual(st.queue, ["a", "c", "late"]);
// replying to the last task wraps to the head; replying to the only one empties it.
st.focusId = "late"; waiting = ["a", "c"];
await replyAdvance("late");
assert.strictEqual(st.focusId, "a");
st.queue = ["a"]; st.focusId = "a"; waiting = [];
await replyAdvance("a");
assert.strictEqual(st.focusId, null);
assert.deepStrictEqual(st.queue, []);
})().catch((e) => { console.error(e); process.exit(1); });
`
	f := filepath.Join(t.TempDir(), "t.js")
	src := pre + ui[a:b+len(endMark)] + tests
	if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
