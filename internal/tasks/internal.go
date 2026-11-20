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

// DefaultStandingChatTurns is THE limit on how many recent turns (operator
// + planner messages; internal rows never count) the standing Planner chat
// shows in the UI and replays in the planner's wake prompt. Older messages
// are kept in storage; the planner folds them into its running summary.
// Override with factoryapp.Config.StandingChatTurns / FACTORY_STANDING_CHAT_TURNS.
const DefaultStandingChatTurns = 10
