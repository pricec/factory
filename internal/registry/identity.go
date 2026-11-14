package registry

import "strings"

// ForemanPrefix is the inbox-id prefix every foreman identity carries
// (spec §7.3): a foreman pairing as "server" is addressed everywhere as
// "foreman:server".
const ForemanPrefix = "foreman:"

// InboxID normalizes a foreman reference to full inbox-id form (spec
// §7.3): a bare id ("server") gains the foreman: prefix; an
// already-prefixed reference ("foreman:server") passes through. Paths
// that accept a foreman id from a caller (the MCP message_foreman tool
// and its UI twin, spec §9.4, §6.9) must route through here so a full
// id - the form list_foremen reports - is never double-prefixed into
// "foreman:foreman:server", an inbox no consumer drains.
func InboxID(ref string) string {
	if strings.HasPrefix(ref, ForemanPrefix) {
		return ref
	}
	return ForemanPrefix + ref
}

// BareID strips the foreman: prefix when present ("foreman:server" ->
// "server").
func BareID(id string) string {
	return strings.TrimPrefix(id, ForemanPrefix)
}

// SameForeman reports whether two foreman references name the same
// foreman, accepting either form on either side. The registry stores
// full inbox ids in production (pairing upserts by identity, spec §6.7)
// while tests may seed bare ids, so comparison runs on the bare form.
func SameForeman(a, b string) bool {
	return a != "" && b != "" && BareID(a) == BareID(b)
}
