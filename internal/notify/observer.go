package notify

import "time"

// Observer receives liveness events (spec §4.7). Storage of liveness state
// lives in internal/jobs (#9), which subscribes; this package only emits.
//
// identity is the inbox id of the connected client (e.g. "foreman:alpha");
// for foremen streaming their own inbox, that is the foreman identity.
type Observer interface {
	// OnActivity fires on any authenticated request from identity, at ts.
	OnActivity(identity string, ts time.Time)
	// OnConnect fires when identity opens an SSE stream.
	OnConnect(identity string)
	// OnDisconnect fires when identity's SSE stream drops.
	OnDisconnect(identity string)
}
