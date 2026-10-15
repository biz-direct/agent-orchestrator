package crews

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Crew messages: durable role-to-role messages on a task crew.
//
// A message is a row before it is anything else. Sending records it (once per
// idempotency key) and then asks the dispatcher to deliver it; the dispatcher
// claims each row, hands it to the target's chat with the row id as the client
// message id, and marks it delivered. Because the chat layer dedupes on that id
// and the claim is an atomic state change, a retry, a concurrent request, or a
// restart can repeat any step without delivering twice.

const (
	// MaxMessageBytes bounds one message body.
	MaxMessageBytes = 16 * 1024
	// DefaultRateLimit plain messages per sender/target pair per DefaultRateWindow
	// are delivered; later ones are held for a person.
	DefaultRateLimit = 10
	// DefaultRateWindow is the window DefaultRateLimit applies to.
	DefaultRateWindow = 10 * time.Minute
	// maxDeliveryAttempts bounds retries of a delivery that keeps erroring.
	maxDeliveryAttempts = 5
	// staleClaimAfter is how long a claim may sit before another pass retries it.
	staleClaimAfter = 2 * time.Minute

	waitingBlockedReason = "waiting: the target is blocked on a permission prompt"
)

// MessageStore is the durable crew message surface.
type MessageStore interface {
	FindCrewRoleSession(ctx context.Context, owner domain.SessionID, role string) (domain.SessionID, bool, error)
	CreateCrewMessage(ctx context.Context, msg domain.CrewMessage, rateLimit int, window time.Duration) (domain.CrewMessage, bool, error)
	GetCrewMessage(ctx context.Context, id string) (domain.CrewMessage, bool, error)
	ListCrewMessages(ctx context.Context, owner domain.SessionID) ([]domain.CrewMessage, error)
	ListCrewMessageTargetsWithPending(ctx context.Context) ([]domain.SessionID, error)
	ListQueuedCrewMessagesForTarget(ctx context.Context, target domain.SessionID) ([]domain.CrewMessage, error)
	ClaimCrewMessage(ctx context.Context, id string, now time.Time) (bool, error)
	MarkCrewMessageDelivered(ctx context.Context, id string, now time.Time) (bool, error)
	RequeueCrewMessage(ctx context.Context, id, reason string, now time.Time) (bool, error)
	FailCrewMessage(ctx context.Context, id, reason string, now time.Time) (bool, error)
	RequeueStaleCrewMessages(ctx context.Context, before, now time.Time) (int64, error)
	SetCrewMessageWaitReason(ctx context.Context, id, reason string) error
	EditCrewMessage(ctx context.Context, id, body string, now time.Time) (bool, error)
	DropCrewMessage(ctx context.Context, id string, now time.Time) (bool, error)
	ReleaseCrewMessage(ctx context.Context, id string, now time.Time) (bool, error)
}

// messaging is the optional crew message wiring of a Service.
type messaging struct {
	store     MessageStore
	deliverer ports.CrewMessageDeliverer
	clock     func() time.Time
	newID     func() string
	// headCommit resolves the owner worktree's current commit ("" when unknown).
	headCommit func(ctx context.Context, workspace string) string
	// baton resolves the task's current baton holder ("" when no workflow
	// assigns one).
	baton      func(ctx context.Context, owner domain.SessionID) string
	rateLimit  int
	rateWindow time.Duration
	// targets serializes dispatch passes per target inside this process so a
	// target's messages keep their send order; the row claim is what makes
	// delivery exactly-once across processes.
	targets sync.Map
}

// MessageOptions tunes crew messaging. Zero values use the defaults.
type MessageOptions struct {
	Clock      func() time.Time
	NewID      func() string
	HeadCommit func(ctx context.Context, workspace string) string
	Baton      func(ctx context.Context, owner domain.SessionID) string
	RateLimit  int
	RateWindow time.Duration
}

// WithMessages enables crew messages. It needs role sessions too.
func (s *Service) WithMessages(store MessageStore, deliverer ports.CrewMessageDeliverer, opts MessageOptions) *Service {
	m := &messaging{
		store: store, deliverer: deliverer, clock: opts.Clock, newID: opts.NewID,
		headCommit: opts.HeadCommit, baton: opts.Baton, rateLimit: opts.RateLimit, rateWindow: opts.RateWindow,
	}
	if m.clock == nil {
		m.clock = time.Now
	}
	if m.newID == nil {
		m.newID = uuid.NewString
	}
	if m.headCommit == nil {
		m.headCommit = gitHeadCommit
	}
	if m.baton == nil {
		// Read the baton lazily: WithBaton may be applied after WithMessages.
		m.baton = func(ctx context.Context, owner domain.SessionID) string {
			if s.baton == nil {
				return ""
			}
			h, err := s.baton.GetCrewBaton(ctx, owner)
			if err != nil {
				return ""
			}
			if h.Kind == domain.BatonHolderRole {
				return h.Role
			}
			return string(h.Kind)
		}
	}
	if m.rateLimit == 0 {
		m.rateLimit = DefaultRateLimit
	}
	if m.rateWindow == 0 {
		m.rateWindow = DefaultRateWindow
	}
	s.msgs = m
	return s
}

func gitHeadCommit(ctx context.Context, workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := aoprocess.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SendMessageInput is the body of POST /sessions/{id}/crew/messages. The path
// session is the sender's own role session.
type SendMessageInput struct {
	// To is the target role name, within the sender's own crew.
	To      string `json:"to"`
	Message string `json:"message"`
	// Steer adds the message to the target's running turn instead of queueing
	// behind it.
	Steer bool `json:"steer,omitempty"`
	// IdempotencyKey makes a retried send land once. Retrying with the same key
	// returns the original message.
	IdempotencyKey string `json:"idempotencyKey"`
}

// EditMessageInput is the body of PATCH /sessions/{id}/crew/messages/{messageId}.
type EditMessageInput struct {
	Message string `json:"message"`
}

// CrewMessage is the API view of one message in the crew thread.
type CrewMessage struct {
	ID          string    `json:"id"`
	Seq         int64     `json:"seq"`
	SenderRole  string    `json:"senderRole"`
	TargetRole  string    `json:"targetRole"`
	Message     string    `json:"message"`
	Mode        string    `json:"mode" enum:"queue,steer"`
	Commit      string    `json:"commit,omitempty"`
	BatonHolder string    `json:"batonHolder,omitempty"`
	State       string    `json:"state" enum:"queued,held,delivering,delivered,dropped,failed"`
	StateReason string    `json:"stateReason,omitempty"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"createdAt"`
	DeliveredAt time.Time `json:"deliveredAt,omitempty"`
	// Editable is true while a person can still edit, drop, or (for held) release it.
	Editable bool `json:"editable"`
	// Duplicate is true on a send that returned an already recorded message.
	Duplicate bool `json:"duplicate,omitempty"`
}

func messageView(m domain.CrewMessage) CrewMessage {
	return CrewMessage{
		ID: m.ID, Seq: m.Seq, SenderRole: m.SenderRole, TargetRole: m.TargetRole, Message: m.Body,
		Mode: string(m.Mode), Commit: m.CommitSHA, BatonHolder: m.BatonHolder, State: string(m.State),
		StateReason: m.StateReason, Attempts: m.Attempts, CreatedAt: m.CreatedAt, DeliveredAt: m.DeliveredAt,
		Editable: m.State == domain.CrewMessageQueued || m.State == domain.CrewMessageHeld,
	}
}

func (s *Service) messagesAvailable() error {
	if s.msgs == nil || s.sessions == nil {
		return apierr.Unavailable("CREW_MESSAGES_UNAVAILABLE", "Crew messages are not available in this build")
	}
	return nil
}

// SendMessage implements Manager.
func (s *Service) SendMessage(ctx context.Context, senderID domain.SessionID, in SendMessageInput) (CrewMessage, error) {
	if err := s.messagesAvailable(); err != nil {
		return CrewMessage{}, err
	}
	m := s.msgs
	body := strings.TrimSpace(in.Message)
	to := strings.TrimSpace(in.To)
	key := strings.TrimSpace(in.IdempotencyKey)
	switch {
	case to == "":
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_TARGET_REQUIRED", "A target role is required", nil)
	case body == "":
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_EMPTY", "A message is required", nil)
	case !utf8.ValidString(body) || len(body) > MaxMessageBytes:
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_TOO_LONG", fmt.Sprintf("A message must be valid text of at most %d bytes", MaxMessageBytes), nil)
	}
	if key == "" {
		key = m.newID()
	}
	senderRole, ownerID, err := s.senderRole(ctx, senderID)
	if err != nil {
		return CrewMessage{}, err
	}
	if to == senderRole {
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_SELF", "A role cannot message itself", nil)
	}
	// Roles are addressed by name inside the sender's own crew. A role of another
	// task is simply not found here, so cross-crew delivery has no route.
	targetID, found, err := m.store.FindCrewRoleSession(ctx, ownerID, to)
	if err != nil {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to resolve the target role")
	}
	if found {
		if rec, ok, gerr := s.sessions.GetSession(ctx, targetID); gerr != nil {
			return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to resolve the target role")
		} else if !ok || rec.IsTerminated {
			found = false
		}
	}
	if !found {
		return CrewMessage{}, apierr.NotFound("CREW_MESSAGE_TARGET_NOT_IN_CREW",
			fmt.Sprintf("Role %q is not in your crew; messages only reach roles on the same task", to))
	}
	owner, ok, err := s.sessions.GetSession(ctx, ownerID)
	if err != nil || !ok {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to load the task")
	}
	mode := domain.CrewMessageModeQueue
	if in.Steer {
		mode = domain.CrewMessageModeSteer
	}
	baton := ""
	if m.baton != nil {
		baton = m.baton(ctx, ownerID)
	}
	stored, duplicate, err := m.store.CreateCrewMessage(ctx, domain.CrewMessage{
		ID: m.newID(), OwnerSessionID: ownerID, SenderRole: senderRole, SenderSessionID: senderID,
		TargetRole: to, TargetSessionID: targetID, IdempotencyKey: key, Body: body, Mode: mode,
		CommitSHA: m.headCommit(ctx, owner.Metadata.WorkspacePath), BatonHolder: baton,
		CreatedAt: m.clock().UTC(),
	}, m.rateLimit, m.rateWindow)
	if err != nil {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to record the message")
	}
	if duplicate && (stored.TargetRole != to || stored.Mode != mode) {
		return CrewMessage{}, apierr.Conflict("CREW_MESSAGE_KEY_REUSED", domain.ErrCrewMessageKeyConflict.Error(), nil)
	}
	// Delivery is best effort here: the row is durable, and the sweeper retries.
	s.dispatchTarget(ctx, stored.TargetSessionID)
	if current, found, gerr := m.store.GetCrewMessage(ctx, stored.ID); gerr == nil && found {
		stored = current
	}
	view := messageView(stored)
	view.Duplicate = duplicate
	return view, nil
}

// senderRole resolves a role session to its role name and owning task.
func (s *Service) senderRole(ctx context.Context, id domain.SessionID) (string, domain.SessionID, error) {
	rec, ok, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return "", "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok {
		return "", "", apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	role, err := s.sessions.GetSessionCrewRole(ctx, id)
	if err != nil {
		return "", "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	owner, err := s.sessions.GetSessionAttachedTo(ctx, id)
	if err != nil {
		return "", "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if role == "" || owner == "" {
		return "", "", apierr.Invalid("CREW_MESSAGE_SENDER_NOT_ROLE", "Only a crew role session can message teammates", nil)
	}
	if rec.IsTerminated {
		return "", "", apierr.Conflict("CREW_MESSAGE_SENDER_ENDED", "The sending role session has ended", nil)
	}
	return role, owner, nil
}

// threadOwner resolves the task a thread request addresses: the task itself or
// any of its role sessions.
func (s *Service) threadOwner(ctx context.Context, id domain.SessionID) (domain.SessionID, error) {
	if _, ok, err := s.sessions.GetSession(ctx, id); err != nil {
		return "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	} else if !ok {
		return "", apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	attachedTo, err := s.sessions.GetSessionAttachedTo(ctx, id)
	if err != nil {
		return "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if attachedTo != "" {
		return attachedTo, nil
	}
	return id, nil
}

// ListMessages implements Manager.
func (s *Service) ListMessages(ctx context.Context, id domain.SessionID) ([]CrewMessage, error) {
	if err := s.messagesAvailable(); err != nil {
		return nil, err
	}
	owner, err := s.threadOwner(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.msgs.store.ListCrewMessages(ctx, owner)
	if err != nil {
		return nil, apierr.Internal("CREW_MESSAGES_LOAD_FAILED", "Failed to load the crew thread")
	}
	out := make([]CrewMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, messageView(row))
	}
	return out, nil
}

// personMessage loads a message a person is acting on and checks it belongs to
// the addressed task.
func (s *Service) personMessage(ctx context.Context, id domain.SessionID, messageID string) (domain.CrewMessage, error) {
	if err := s.messagesAvailable(); err != nil {
		return domain.CrewMessage{}, err
	}
	owner, err := s.threadOwner(ctx, id)
	if err != nil {
		return domain.CrewMessage{}, err
	}
	msg, found, err := s.msgs.store.GetCrewMessage(ctx, messageID)
	if err != nil {
		return domain.CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to load the message")
	}
	if !found || msg.OwnerSessionID != owner {
		return domain.CrewMessage{}, apierr.NotFound("CREW_MESSAGE_NOT_FOUND", "Unknown crew message")
	}
	return msg, nil
}

func notPending() error {
	return apierr.Conflict("CREW_MESSAGE_NOT_PENDING", domain.ErrCrewMessageNotPending.Error(), nil)
}

// EditMessage implements Manager. A person may rewrite a message until it is claimed for delivery.
func (s *Service) EditMessage(ctx context.Context, id domain.SessionID, messageID string, in EditMessageInput) (CrewMessage, error) {
	msg, err := s.personMessage(ctx, id, messageID)
	if err != nil {
		return CrewMessage{}, err
	}
	body := strings.TrimSpace(in.Message)
	switch {
	case body == "":
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_EMPTY", "A message is required", nil)
	case !utf8.ValidString(body) || len(body) > MaxMessageBytes:
		return CrewMessage{}, apierr.Invalid("CREW_MESSAGE_TOO_LONG", fmt.Sprintf("A message must be valid text of at most %d bytes", MaxMessageBytes), nil)
	}
	ok, err := s.msgs.store.EditCrewMessage(ctx, msg.ID, body, s.msgs.clock().UTC())
	if err != nil {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to edit the message")
	}
	if !ok {
		return CrewMessage{}, notPending()
	}
	return s.reload(ctx, msg.ID)
}

// DropMessage implements Manager.
func (s *Service) DropMessage(ctx context.Context, id domain.SessionID, messageID string) (CrewMessage, error) {
	msg, err := s.personMessage(ctx, id, messageID)
	if err != nil {
		return CrewMessage{}, err
	}
	ok, err := s.msgs.store.DropCrewMessage(ctx, msg.ID, s.msgs.clock().UTC())
	if err != nil {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to drop the message")
	}
	if !ok {
		return CrewMessage{}, notPending()
	}
	return s.reload(ctx, msg.ID)
}

// ReleaseMessage implements Manager. It moves a message held for a person into delivery.
func (s *Service) ReleaseMessage(ctx context.Context, id domain.SessionID, messageID string) (CrewMessage, error) {
	msg, err := s.personMessage(ctx, id, messageID)
	if err != nil {
		return CrewMessage{}, err
	}
	ok, err := s.msgs.store.ReleaseCrewMessage(ctx, msg.ID, s.msgs.clock().UTC())
	if err != nil {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to release the message")
	}
	if !ok {
		return CrewMessage{}, notPending()
	}
	s.dispatchTarget(ctx, msg.TargetSessionID)
	return s.reload(ctx, msg.ID)
}

func (s *Service) reload(ctx context.Context, id string) (CrewMessage, error) {
	msg, found, err := s.msgs.store.GetCrewMessage(ctx, id)
	if err != nil || !found {
		return CrewMessage{}, apierr.Internal("CREW_MESSAGE_FAILED", "Failed to load the message")
	}
	return messageView(msg), nil
}

// DeliveryText is the turn the target receives: the body, tagged with who sent
// it, who holds the baton, and the commit it was sent at.
func DeliveryText(m domain.CrewMessage) string {
	tag := "from " + m.SenderRole
	if m.BatonHolder != "" {
		tag += " · baton: " + m.BatonHolder
	}
	if m.CommitSHA != "" {
		sha := m.CommitSHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		tag += " · commit " + sha
	}
	return fmt.Sprintf("[crew message %s]\n%s\n\n(To reply to %s, run `ao crew send --to %s --message \"...\"`; chat text is not delivered to teammates.)",
		tag, m.Body, m.SenderRole, m.SenderRole)
}

// Recover requeues claims a previous daemon process left half-delivered and
// delivers everything pending. It is safe to run at any time.
func (s *Service) Recover(ctx context.Context) {
	if s.msgs == nil {
		return
	}
	now := s.msgs.clock().UTC()
	if _, err := s.msgs.store.RequeueStaleCrewMessages(ctx, now, now); err != nil {
		slog.Default().Warn("crew messages: requeue interrupted deliveries failed", "err", err)
	}
	s.DeliverPending(ctx)
}

// DeliverPending runs one dispatch pass over every target with queued messages.
func (s *Service) DeliverPending(ctx context.Context) {
	if s.msgs == nil {
		return
	}
	now := s.msgs.clock().UTC()
	if _, err := s.msgs.store.RequeueStaleCrewMessages(ctx, now.Add(-staleClaimAfter), now); err != nil {
		slog.Default().Warn("crew messages: requeue stale deliveries failed", "err", err)
	}
	targets, err := s.msgs.store.ListCrewMessageTargetsWithPending(ctx)
	if err != nil {
		slog.Default().Warn("crew messages: list pending targets failed", "err", err)
		return
	}
	for _, target := range targets {
		s.dispatchTarget(ctx, target)
	}
}

// RunMessageDispatcher retries pending deliveries (a target that was blocked, a
// failed attempt) until ctx ends.
func (s *Service) RunMessageDispatcher(ctx context.Context, interval time.Duration) {
	if s.msgs == nil {
		return
	}
	s.Recover(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.DeliverPending(ctx)
		}
	}
}

// dispatchTarget delivers a target's queued messages in send order. It stops at
// the first message that cannot go yet so later messages never overtake it.
func (s *Service) dispatchTarget(ctx context.Context, target domain.SessionID) {
	m := s.msgs
	if m == nil || m.deliverer == nil {
		return
	}
	lock, _ := m.targets.LoadOrStore(target, &sync.Mutex{})
	mu, ok := lock.(*sync.Mutex)
	if !ok {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	log := slog.Default().With("target", target)
	queued, err := m.store.ListQueuedCrewMessagesForTarget(ctx, target)
	if err != nil {
		log.Warn("crew messages: list queued failed", "err", err)
		return
	}
	for _, msg := range queued {
		rec, ok, err := s.sessions.GetSession(ctx, target)
		if err != nil {
			log.Warn("crew messages: load target failed", "err", err)
			return
		}
		now := m.clock().UTC()
		if !ok || rec.IsTerminated {
			s.failMessage(ctx, msg.ID, "the target role session has ended")
			continue
		}
		// A permission prompt is a pending decision: never inject input behind it.
		if rec.Activity.State == domain.ActivityBlocked {
			if err := m.store.SetCrewMessageWaitReason(ctx, msg.ID, waitingBlockedReason); err != nil {
				log.Warn("crew messages: note wait reason failed", "err", err)
			}
			return
		}
		claimed, err := m.store.ClaimCrewMessage(ctx, msg.ID, now)
		if err != nil {
			log.Warn("crew messages: claim failed", "err", err)
			return
		}
		if !claimed {
			continue // another pass owns it, or a person dropped/edited it
		}
		// Re-read after the claim so a person's last-moment edit is what is sent.
		current, found, err := m.store.GetCrewMessage(ctx, msg.ID)
		if err != nil || !found {
			_, _ = m.store.RequeueCrewMessage(ctx, msg.ID, "could not reload the message; retrying", now)
			return
		}
		derr := m.deliverer.DeliverCrewMessage(ctx, ports.CrewMessageDelivery{
			Target: target, Sender: current.SenderSessionID, Text: DeliveryText(current),
			ClientMessageID: current.ID, Steer: current.Mode == domain.CrewMessageModeSteer,
		})
		switch {
		case derr == nil:
			if _, err := m.store.MarkCrewMessageDelivered(ctx, current.ID, m.clock().UTC()); err != nil {
				log.Warn("crew messages: mark delivered failed; a later pass redelivers idempotently", "err", err)
				return
			}
		case errors.Is(derr, ports.ErrCrewMessageTargetGone):
			s.failMessage(ctx, current.ID, "the target role session has ended")
		case current.Attempts >= maxDeliveryAttempts:
			s.failMessage(ctx, current.ID, "delivery failed: "+derr.Error())
			return
		default:
			log.Warn("crew messages: delivery failed; will retry", "message", current.ID, "err", derr)
			_, _ = m.store.RequeueCrewMessage(ctx, current.ID, "delivery failed; retrying: "+derr.Error(), m.clock().UTC())
			return
		}
	}
}

func (s *Service) failMessage(ctx context.Context, id, reason string) {
	if _, err := s.msgs.store.FailCrewMessage(ctx, id, reason, s.msgs.clock().UTC()); err != nil {
		slog.Default().Warn("crew messages: mark failed failed", "message", id, "err", err)
	}
}
