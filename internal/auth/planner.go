package auth

import "context"

// FactoryPlannerIdentity is the factory planner credential identity (spec
// §9.4): the credential that authorizes only the planner MCP surface at
// /v1/mcp/factory. It carries no inbox scopes - the planner touches no
// inboxes directly (message_foreman posts via the factory's own inbox
// service, attributed to this identity).
const FactoryPlannerIdentity = "factory:planner"

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
