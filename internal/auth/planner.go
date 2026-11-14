package auth

import "context"

// FactoryPlannerIdentity is the factory planner credential identity (spec
// §9.4): the credential that authorizes only the planner MCP surface at
// /v1/mcp/factory. It carries no inbox scopes - the planner touches no
// inboxes directly (message_foreman posts via the factory's own inbox
// service, attributed to this identity).
const FactoryPlannerIdentity = "factory:planner"

// FactoryBrainIdentity is the LLM planner's envelope sender identity (spec
// §9.5): brain.reply envelopes posted into the operator inbox carry it.
const FactoryBrainIdentity = "factory:brain"

// HumanOperatorIdentity is the conversational UI's sender identity (spec
// §9.5): chat.message envelopes posted via POST /v1/chat carry it.
const HumanOperatorIdentity = "human:operator"

// PlannerMainInbox is the operator inbox (spec §9.5): the single inbox the
// conversational UI renders - brain replies, job notifications, and the
// operator's own chat messages all land here. Drained (acked) by the
// planner loop; history stays replayable via GET /v1/inboxes/planner:main.
const PlannerMainInbox = "planner:main"

// MintPlannerToken mints a factory planner credential (spec §9.4): identity
// factory:planner and NO inbox scopes. It authorizes only the planner MCP
// surface, never inbox reads or posts; the surface's middleware checks the
// identity directly. A library call, not an HTTP endpoint. inboxID is
// accepted for signature symmetry with the other mint calls and recorded
// nowhere - the credential is deliberately scope-free.
func (s *Store) MintPlannerToken(ctx context.Context, inboxID string) (bearer string, err error) {
	bearer = randomHex()
	if err := s.insertCredential(ctx, bearer, FactoryPlannerIdentity, "", nil); err != nil {
		return "", err
	}
	return bearer, nil
}
