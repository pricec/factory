package tasks

// Operator-visible envelope types (spec §9.5). Everything else is internal.
const (
	TypeChatMessage = "chat.message"
	TypeBrainReply  = "brain.reply"
)

// IsInternal reports whether an envelope of type typ is internal, i.e.
// hidden from the operator's default task view. "Internal" is derived from
// the type and never stored: ONLY chat.message and brain.reply are
// operator-visible; foreman.report, brain.failed, notify.*, job.* and any
// unknown type are internal.
func IsInternal(typ string) bool {
	return typ != TypeChatMessage && typ != TypeBrainReply
}
