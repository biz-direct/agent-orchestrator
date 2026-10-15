package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrCrewMessageTargetGone means the target role's conversation has ended, so
// the message can never be delivered.
var ErrCrewMessageTargetGone = errors.New("the target role session has ended")

// CrewMessageDelivery is one crew message handed to a role's conversation.
type CrewMessageDelivery struct {
	Target domain.SessionID
	Sender domain.SessionID
	Text   string
	// ClientMessageID is the message's stable id. The chat layer dedupes on it,
	// so a delivery repeated after a crash or a concurrent dispatch lands once.
	ClientMessageID string
	// Steer adds the message to the target's running turn; otherwise it queues
	// behind that turn. Either way an idle target starts a new turn.
	Steer bool
}

// CrewMessageDeliverer delivers crew messages into role conversations. It
// reuses the existing session messaging semantics.
type CrewMessageDeliverer interface {
	DeliverCrewMessage(ctx context.Context, d CrewMessageDelivery) error
}
