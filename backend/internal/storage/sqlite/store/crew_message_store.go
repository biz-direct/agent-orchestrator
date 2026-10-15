package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func crewMessageFromGen(row gen.CrewMessage) domain.CrewMessage {
	m := domain.CrewMessage{
		Seq: row.Seq, ID: row.ID, OwnerSessionID: domain.SessionID(row.OwnerSessionID),
		SenderRole: row.SenderRole, SenderSessionID: domain.SessionID(row.SenderSessionID),
		TargetRole: row.TargetRole, TargetSessionID: domain.SessionID(row.TargetSessionID),
		IdempotencyKey: row.IdempotencyKey, Body: row.Body, Mode: domain.CrewMessageMode(row.Mode),
		CommitSHA: row.CommitSha, BatonHolder: row.BatonHolder, State: domain.CrewMessageState(row.State),
		StateReason: row.StateReason, Attempts: int(row.Attempts), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.DeliveredAt.Valid {
		m.DeliveredAt = row.DeliveredAt.Time
	}
	return m
}

// CreateCrewMessage records a message exactly once per idempotency key. The
// second return value is true when the key already existed, in which case the
// stored message is returned unchanged. A message beyond rateLimit plain
// messages for its pair within window is stored Held for a person instead of
// Queued. The count and the insert share one write transaction, so concurrent
// sends cannot both slip under the limit.
func (s *Store) CreateCrewMessage(ctx context.Context, msg domain.CrewMessage, rateLimit int, window time.Duration) (domain.CrewMessage, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out gen.CrewMessage
	duplicate := false
	err := s.inTx(ctx, "create crew message", func(q *gen.Queries) error {
		existing, err := q.GetCrewMessageByKey(ctx, gen.GetCrewMessageByKeyParams{
			OwnerSessionID: string(msg.OwnerSessionID), SenderRole: msg.SenderRole, IdempotencyKey: msg.IdempotencyKey,
		})
		if err == nil {
			out, duplicate = existing, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		state, reason := domain.CrewMessageQueued, ""
		if rateLimit > 0 {
			recent, err := q.CountCrewMessagesForPairSince(ctx, gen.CountCrewMessagesForPairSinceParams{
				OwnerSessionID: string(msg.OwnerSessionID), SenderRole: msg.SenderRole, TargetRole: msg.TargetRole,
				Since: msg.CreatedAt.Add(-window),
			})
			if err != nil {
				return err
			}
			if recent >= int64(rateLimit) {
				state = domain.CrewMessageHeld
				reason = fmt.Sprintf("rate limit: %d messages from %s to %s within %s; held for a person", rateLimit, msg.SenderRole, msg.TargetRole, window)
			}
		}
		out, err = q.InsertCrewMessage(ctx, gen.InsertCrewMessageParams{
			ID: msg.ID, OwnerSessionID: string(msg.OwnerSessionID), SenderRole: msg.SenderRole,
			SenderSessionID: string(msg.SenderSessionID), TargetRole: msg.TargetRole,
			TargetSessionID: string(msg.TargetSessionID), IdempotencyKey: msg.IdempotencyKey, Body: msg.Body,
			Mode: string(msg.Mode), CommitSha: msg.CommitSHA, BatonHolder: msg.BatonHolder,
			State: string(state), StateReason: reason, CreatedAt: msg.CreatedAt,
		})
		return err
	})
	if err != nil {
		return domain.CrewMessage{}, false, fmt.Errorf("create crew message: %w", err)
	}
	return crewMessageFromGen(out), duplicate, nil
}

// GetCrewMessage returns one message by id.
func (s *Store) GetCrewMessage(ctx context.Context, id string) (domain.CrewMessage, bool, error) {
	row, err := s.qr.GetCrewMessage(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CrewMessage{}, false, nil
	}
	if err != nil {
		return domain.CrewMessage{}, false, fmt.Errorf("get crew message: %w", err)
	}
	return crewMessageFromGen(row), true, nil
}

// ListCrewMessages returns a task's whole crew thread in send order.
func (s *Store) ListCrewMessages(ctx context.Context, owner domain.SessionID) ([]domain.CrewMessage, error) {
	rows, err := s.qr.ListCrewMessages(ctx, string(owner))
	if err != nil {
		return nil, fmt.Errorf("list crew messages: %w", err)
	}
	return crewMessagesFromGen(rows), nil
}

// ListCrewMessageTargetsWithPending returns every target session with a queued message.
func (s *Store) ListCrewMessageTargetsWithPending(ctx context.Context) ([]domain.SessionID, error) {
	rows, err := s.qr.ListCrewMessageTargetsWithPending(ctx)
	if err != nil {
		return nil, fmt.Errorf("list crew message targets: %w", err)
	}
	out := make([]domain.SessionID, 0, len(rows))
	for _, id := range rows {
		out = append(out, domain.SessionID(id))
	}
	return out, nil
}

// ListQueuedCrewMessagesForTarget returns a target's queued messages in send order.
func (s *Store) ListQueuedCrewMessagesForTarget(ctx context.Context, target domain.SessionID) ([]domain.CrewMessage, error) {
	rows, err := s.qr.ListQueuedCrewMessagesForTarget(ctx, string(target))
	if err != nil {
		return nil, fmt.Errorf("list queued crew messages: %w", err)
	}
	return crewMessagesFromGen(rows), nil
}

func crewMessagesFromGen(rows []gen.CrewMessage) []domain.CrewMessage {
	out := make([]domain.CrewMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, crewMessageFromGen(row))
	}
	return out
}

// ClaimCrewMessage moves a queued message to delivering for exactly one caller.
func (s *Store) ClaimCrewMessage(ctx context.Context, id string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "claim", func(q *gen.Queries) (int64, error) {
		return q.ClaimCrewMessage(ctx, gen.ClaimCrewMessageParams{ID: id, Now: now})
	})
}

// MarkCrewMessageDelivered records a claimed message as delivered.
func (s *Store) MarkCrewMessageDelivered(ctx context.Context, id string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "mark delivered", func(q *gen.Queries) (int64, error) {
		return q.MarkCrewMessageDelivered(ctx, gen.MarkCrewMessageDeliveredParams{ID: id, Now: sql.NullTime{Time: now, Valid: true}})
	})
}

// RequeueCrewMessage returns a claimed message to the queue after a failed attempt.
func (s *Store) RequeueCrewMessage(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "requeue", func(q *gen.Queries) (int64, error) {
		return q.RequeueCrewMessage(ctx, gen.RequeueCrewMessageParams{ID: id, StateReason: reason, Now: now})
	})
}

// FailCrewMessage abandons delivery of a queued or claimed message.
func (s *Store) FailCrewMessage(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "fail", func(q *gen.Queries) (int64, error) {
		return q.FailCrewMessage(ctx, gen.FailCrewMessageParams{ID: id, StateReason: reason, Now: now})
	})
}

// RequeueStaleCrewMessages returns claims older than before to the queue. A
// claim older than any live dispatch pass belongs to a crashed daemon; the
// retry is safe because the chat layer dedupes on the message id.
func (s *Store) RequeueStaleCrewMessages(ctx context.Context, before, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RequeueStaleCrewMessages(ctx, gen.RequeueStaleCrewMessagesParams{Before: before, Now: now})
	if err != nil {
		return 0, fmt.Errorf("requeue stale crew messages: %w", err)
	}
	return n, nil
}

// SetCrewMessageWaitReason notes why a queued message is waiting.
func (s *Store) SetCrewMessageWaitReason(ctx context.Context, id, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.SetCrewMessageWaitReason(ctx, gen.SetCrewMessageWaitReasonParams{ID: id, StateReason: reason}); err != nil {
		return fmt.Errorf("set crew message wait reason: %w", err)
	}
	return nil
}

// EditCrewMessage replaces the body of a queued or held message.
func (s *Store) EditCrewMessage(ctx context.Context, id, body string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "edit", func(q *gen.Queries) (int64, error) {
		return q.EditCrewMessage(ctx, gen.EditCrewMessageParams{ID: id, Body: body, Now: now})
	})
}

// DropCrewMessage drops a queued or held message.
func (s *Store) DropCrewMessage(ctx context.Context, id string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "drop", func(q *gen.Queries) (int64, error) {
		return q.DropCrewMessage(ctx, gen.DropCrewMessageParams{ID: id, Now: now})
	})
}

// ReleaseCrewMessage moves a held message into the delivery queue.
func (s *Store) ReleaseCrewMessage(ctx context.Context, id string, now time.Time) (bool, error) {
	return s.crewMessageWrite(ctx, "release", func(q *gen.Queries) (int64, error) {
		return q.ReleaseCrewMessage(ctx, gen.ReleaseCrewMessageParams{ID: id, Now: now})
	})
}

// crewMessageWrite runs one guarded UPDATE and reports whether it matched a row.
func (s *Store) crewMessageWrite(ctx context.Context, what string, fn func(*gen.Queries) (int64, error)) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := fn(s.qw)
	if err != nil {
		return false, fmt.Errorf("%s crew message: %w", what, err)
	}
	return n > 0, nil
}
