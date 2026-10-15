package domain

import (
	"errors"
	"time"
)

// CrewMessageState is where a crew message is in its delivery.
type CrewMessageState string

// Crew message states. A message is editable and droppable by a person only
// while it is Queued or Held; once Delivering it belongs to the dispatcher.
const (
	// CrewMessageQueued waits for the dispatcher. It may also be waiting on
	// the target (blocked on a permission prompt); StateReason says so.
	CrewMessageQueued CrewMessageState = "queued"
	// CrewMessageHeld is parked for a person: the sender exceeded the plain
	// message rate limit for the pair.
	CrewMessageHeld CrewMessageState = "held"
	// CrewMessageDelivering is claimed by exactly one dispatcher pass.
	CrewMessageDelivering CrewMessageState = "delivering"
	CrewMessageDelivered  CrewMessageState = "delivered"
	CrewMessageDropped    CrewMessageState = "dropped"
	// CrewMessageFailed means delivery was abandoned (the target ended, or
	// retries were exhausted). StateReason says why.
	CrewMessageFailed CrewMessageState = "failed"
)

// CrewMessageMode mirrors existing session messaging: Queue waits behind the
// target's running turn, Steer adds to it.
type CrewMessageMode string

// Crew message modes.
const (
	CrewMessageModeQueue CrewMessageMode = "queue"
	CrewMessageModeSteer CrewMessageMode = "steer"
)

// CrewMessage is one durable role-to-role message on a task crew.
type CrewMessage struct {
	// Seq orders the whole crew thread and the per-target delivery order.
	Seq int64
	// ID doubles as the chat layer's client message id, which makes delivery
	// exactly-once across retries, concurrent dispatchers, and restarts.
	ID              string
	OwnerSessionID  SessionID
	SenderRole      string
	SenderSessionID SessionID
	TargetRole      string
	TargetSessionID SessionID
	// IdempotencyKey is the sender-chosen retry handle, unique per owner and
	// sender role.
	IdempotencyKey string
	Body           string
	Mode           CrewMessageMode
	// CommitSHA and BatonHolder are the task's state when the message was sent.
	CommitSHA   string
	BatonHolder string
	State       CrewMessageState
	StateReason string
	Attempts    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeliveredAt time.Time
}

var (
	// ErrCrewMessageNotFound means no such message exists.
	ErrCrewMessageNotFound = errors.New("crew message not found")
	// ErrCrewMessageNotPending means a person tried to edit, drop, or release a
	// message that is no longer queued or held.
	ErrCrewMessageNotPending = errors.New("crew message is no longer waiting for delivery")
	// ErrCrewMessageKeyConflict means an idempotency key was reused for a
	// different message.
	ErrCrewMessageKeyConflict = errors.New("crew message idempotency key was used for a different message")
)
