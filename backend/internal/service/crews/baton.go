package crews

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// BatonStore is the durable baton surface. The move is atomic: it reads the
// current holder, checks a role target is live, writes the new holder, and
// appends the history row in one transaction.
type BatonStore interface {
	GetCrewBaton(ctx context.Context, owner domain.SessionID) (domain.BatonHolder, error)
	MoveCrewBaton(ctx context.Context, owner domain.SessionID, to domain.BatonHolder, actor domain.BatonActor, reason string, now time.Time) (from domain.BatonHolder, changed bool, err error)
	ListCrewBatonEvents(ctx context.Context, owner domain.SessionID) ([]domain.BatonEvent, error)
}

// Messenger delivers a message to a session. Baton notices are best effort: the
// durable baton, not the message, is the source of truth.
type Messenger interface {
	Send(ctx context.Context, id domain.SessionID, message string) error
}

// WithBaton enables the write baton. A nil messenger skips the prompt updates.
func (s *Service) WithBaton(store BatonStore, messenger Messenger) *Service {
	s.baton, s.messenger = store, messenger
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// BatonStatus is the body of GET /sessions/{sessionId}/crew/baton.
type BatonStatus struct {
	OwnerSessionID domain.SessionID    `json:"ownerSessionId"`
	Holder         domain.BatonHolder  `json:"holder"`
	History        []domain.BatonEvent `json:"history"`
}

// MoveBatonInput is the body of PUT /sessions/{sessionId}/crew/baton.
type MoveBatonInput struct {
	// To is the new holder: the worker, a live role, or the human.
	To domain.BatonHolder `json:"to"`
	// Reason is recorded with the move.
	Reason string `json:"reason,omitempty"`
}

const maxBatonReason = 500

func (s *Service) batonAvailable() error {
	if s.baton == nil || s.sessions == nil {
		return apierr.Unavailable("CREW_BATON_UNAVAILABLE", "The write baton is not available in this build")
	}
	return nil
}

// GetBaton implements Manager.
func (s *Service) GetBaton(ctx context.Context, ownerID domain.SessionID) (BatonStatus, error) {
	if err := s.batonAvailable(); err != nil {
		return BatonStatus{}, err
	}
	if _, err := s.batonOwner(ctx, ownerID, false); err != nil {
		return BatonStatus{}, err
	}
	holder, err := s.baton.GetCrewBaton(ctx, ownerID)
	if err != nil {
		return BatonStatus{}, apierr.Internal("CREW_BATON_LOAD_FAILED", "Failed to load the write baton")
	}
	history, err := s.baton.ListCrewBatonEvents(ctx, ownerID)
	if err != nil {
		return BatonStatus{}, apierr.Internal("CREW_BATON_LOAD_FAILED", "Failed to load the write baton")
	}
	return BatonStatus{OwnerSessionID: ownerID, Holder: holder, History: history}, nil
}

// MoveBaton implements Manager. It is the human override: the controller
// refuses it for any request attributable to a session.
func (s *Service) MoveBaton(ctx context.Context, ownerID domain.SessionID, in MoveBatonInput) (BatonStatus, error) {
	if err := s.batonAvailable(); err != nil {
		return BatonStatus{}, err
	}
	if err := in.To.Validate(); err != nil {
		return BatonStatus{}, apierr.Invalid("INVALID_CREW_BATON_HOLDER", err.Error(), nil)
	}
	in.To.Role = strings.TrimSpace(in.To.Role)
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > maxBatonReason {
		return BatonStatus{}, apierr.Invalid("INVALID_CREW_BATON_REASON", fmt.Sprintf("Reason must be at most %d characters", maxBatonReason), nil)
	}
	if _, err := s.batonOwner(ctx, ownerID, true); err != nil {
		return BatonStatus{}, err
	}
	if err := s.moveBaton(ctx, ownerID, in.To, domain.BatonActorHuman, reason); err != nil {
		return BatonStatus{}, err
	}
	return s.GetBaton(ctx, ownerID)
}

// batonOwner loads the task the baton belongs to; live is true when the task
// must still be running (a move), false for reads.
func (s *Service) batonOwner(ctx context.Context, id domain.SessionID, live bool) (domain.SessionRecord, error) {
	if live {
		return s.owner(ctx, id)
	}
	rec, ok, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok {
		return domain.SessionRecord{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	return rec, nil
}

// moveBaton performs a move and tells the crew. It is shared by the human
// override and the system hand-back when the holding role is removed.
func (s *Service) moveBaton(ctx context.Context, owner domain.SessionID, to domain.BatonHolder, actor domain.BatonActor, reason string) error {
	from, changed, err := s.baton.MoveCrewBaton(ctx, owner, to, actor, reason, s.now().UTC())
	switch {
	case errors.Is(err, domain.ErrBatonRoleNotLive):
		return apierr.Conflict("CREW_BATON_ROLE_NOT_LIVE", fmt.Sprintf("Role %q has no live session on this task", to.Role), nil)
	case err != nil:
		return apierr.Internal("CREW_BATON_MOVE_FAILED", "Failed to move the write baton")
	}
	if changed {
		s.announceBaton(ctx, owner, from, to, actor, reason)
	}
	return nil
}

// announceBaton tells the worker and every live role whether they may write.
func (s *Service) announceBaton(ctx context.Context, owner domain.SessionID, from, to domain.BatonHolder, actor domain.BatonActor, reason string) {
	if s.messenger == nil {
		return
	}
	notice := func(self domain.BatonHolder) string { return BatonNotice(self, to, from, string(actor), reason) }
	targets := map[domain.SessionID]domain.BatonHolder{owner: {Kind: domain.BatonHolderWorker}}
	ids, err := s.sessions.ListAttachedSessionIDs(ctx, owner)
	if err != nil {
		slog.Default().Warn("crew baton: list roles", "owner", owner, "err", err)
	}
	for _, id := range ids {
		role, err := s.sessions.GetSessionCrewRole(ctx, id)
		if err != nil || role == "" {
			continue
		}
		if rec, ok, err := s.sessions.GetSession(ctx, id); err != nil || !ok || rec.IsTerminated {
			continue
		}
		targets[id] = domain.BatonHolder{Kind: domain.BatonHolderRole, Role: role}
	}
	for id, self := range targets {
		if err := s.messenger.Send(ctx, id, notice(self)); err != nil {
			slog.Default().Warn("crew baton: notify session", "session", id, "err", err)
		}
	}
}

// BatonNotice is the text a crew member receives about the baton. self is the
// recipient's own identity and to is the current holder.
func BatonNotice(self, to, from domain.BatonHolder, actor, reason string) string {
	var b strings.Builder
	if self == to {
		b.WriteString("Crew write baton: you now hold the write baton for this task. You may change code, commit, and push on the task's branch.")
	} else {
		fmt.Fprintf(&b, "Crew write baton: %s now holds the write baton for this task. You are read-only: you may read files, run commands and tests, and discuss, but do not edit, create, or delete files, commit, or push.", to)
	}
	fmt.Fprintf(&b, " (moved from %s by %s", from, actor)
	if reason != "" {
		b.WriteString(": " + domain.SanitizeControlChars(reason))
	}
	b.WriteString(")")
	return b.String()
}

// handBackBaton returns the baton to the worker when the holding role is removed.
func (s *Service) handBackBaton(ctx context.Context, owner domain.SessionID, role string) {
	if s.baton == nil {
		return
	}
	holder, err := s.baton.GetCrewBaton(ctx, owner)
	if err != nil || holder.Kind != domain.BatonHolderRole || holder.Role != role {
		return
	}
	if err := s.moveBaton(ctx, owner, domain.BatonHolder{Kind: domain.BatonHolderWorker}, domain.BatonActorSystem, fmt.Sprintf("role %s was removed", role)); err != nil {
		slog.Default().Warn("crew baton: hand back", "owner", owner, "role", role, "err", err)
	}
}
