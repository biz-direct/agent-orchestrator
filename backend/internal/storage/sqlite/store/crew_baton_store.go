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

// The crew write baton is stored one row per owner session, so a task cannot
// have two holders. Reads resolve the effective holder: a recorded role whose
// session is gone no longer holds anything, and the worker holds by default.

type batonQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// effectiveBaton reads the holder through q (a transaction or the read pool).
func effectiveBaton(ctx context.Context, q batonQuerier, owner domain.SessionID) (domain.BatonHolder, error) {
	holder, _, _, err := rawBaton(ctx, q, owner)
	return holder, err
}

// rawBaton returns the effective holder, and when the stored row is stale (a
// role grant whose session has ended) that stored holder with stale=true.
func rawBaton(ctx context.Context, q batonQuerier, owner domain.SessionID) (effective, stored domain.BatonHolder, stale bool, err error) {
	var kind, role, holderSession string
	err = q.QueryRowContext(ctx, `SELECT holder_kind, holder_role, holder_session_id FROM crew_baton WHERE owner_session_id = ?`, string(owner)).Scan(&kind, &role, &holderSession)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BatonHolder{Kind: domain.BatonHolderWorker}, domain.BatonHolder{}, false, nil
	}
	if err != nil {
		return domain.BatonHolder{}, domain.BatonHolder{}, false, fmt.Errorf("read crew baton: %w", err)
	}
	holder := domain.BatonHolder{Kind: domain.BatonHolderKind(kind), Role: role}
	if holder.Kind == domain.BatonHolderRole {
		// The grant belongs to the session it was made to: once that session has
		// ended the worker holds, and a fresh session for the same role starts
		// read-only until another authorized move.
		if id, live, err := liveRoleSession(ctx, q, owner, role); err != nil {
			return domain.BatonHolder{}, domain.BatonHolder{}, false, err
		} else if !live || string(id) != holderSession {
			return domain.BatonHolder{Kind: domain.BatonHolderWorker}, holder, true, nil
		}
	}
	return holder, holder, false, nil
}

func liveRoleSession(ctx context.Context, q batonQuerier, owner domain.SessionID, role string) (domain.SessionID, bool, error) {
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE attached_for_attempt_id = ? AND is_terminated = 0`,
		domain.CrewRoleMarker(owner, role)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find live crew role: %w", err)
	}
	return domain.SessionID(id), true, nil
}

// GetCrewBaton returns the task's baton holder.
func (s *Store) GetCrewBaton(ctx context.Context, owner domain.SessionID) (domain.BatonHolder, error) {
	return effectiveBaton(ctx, s.readDB, owner)
}

// MoveCrewBaton hands the baton to `to` and records the move, atomically. It
// returns the previous holder; changed is false (and nothing is recorded) when
// `to` already holds it. A role target must have a live session on the task.
func (s *Store) MoveCrewBaton(ctx context.Context, owner domain.SessionID, to domain.BatonHolder, actor domain.BatonActor, reason string, now time.Time) (from domain.BatonHolder, changed bool, err error) {
	if err := to.Validate(); err != nil {
		return domain.BatonHolder{}, false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inTxDB(ctx, "move crew baton", func(_ *gen.Queries, db gen.DBTX) error {
		tx, ok := db.(interface {
			batonQuerier
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		if !ok {
			return errors.New("transaction unavailable")
		}
		effective, stored, stale, err := rawBaton(ctx, tx, owner)
		if err != nil {
			return err
		}
		from = effective
		var toSession domain.SessionID
		if to.Kind == domain.BatonHolderRole {
			id, live, err := liveRoleSession(ctx, tx, owner, to.Role)
			if err != nil {
				return err
			} else if !live {
				return domain.ErrBatonRoleNotLive
			}
			toSession = id
		}
		if stale {
			// The holding role's session ended without a recorded hand-back:
			// persist it now so the row and the history agree.
			if _, err := tx.ExecContext(ctx,
				`UPDATE crew_baton SET holder_kind = 'worker', holder_role = '', holder_session_id = '', updated_at = ? WHERE owner_session_id = ?`,
				now, string(owner)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO crew_baton_events (owner_session_id, from_kind, from_role, to_kind, to_role, actor, reason, created_at) VALUES (?, ?, ?, 'worker', '', 'system', ?, ?)`,
				string(owner), string(stored.Kind), stored.Role, fmt.Sprintf("role %s ended", stored.Role), now); err != nil {
				return err
			}
		}
		if from == to {
			return nil
		}
		changed = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO crew_baton (owner_session_id, holder_kind, holder_role, holder_session_id, updated_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(owner_session_id) DO UPDATE SET holder_kind = excluded.holder_kind, holder_role = excluded.holder_role, holder_session_id = excluded.holder_session_id, updated_at = excluded.updated_at`,
			string(owner), string(to.Kind), to.Role, string(toSession), now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO crew_baton_events (owner_session_id, from_kind, from_role, to_kind, to_role, actor, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			string(owner), string(from.Kind), from.Role, string(to.Kind), to.Role, string(actor), reason, now)
		return err
	})
	return from, changed, err
}

// ListCrewBatonEvents returns the task's baton history, oldest first.
func (s *Store) ListCrewBatonEvents(ctx context.Context, owner domain.SessionID) ([]domain.BatonEvent, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT id, from_kind, from_role, to_kind, to_role, actor, reason, created_at FROM crew_baton_events WHERE owner_session_id = ? ORDER BY id`,
		string(owner))
	if err != nil {
		return nil, fmt.Errorf("list crew baton events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.BatonEvent{}
	for rows.Next() {
		var e domain.BatonEvent
		var fk, tk, actor string
		if err := rows.Scan(&e.ID, &fk, &e.From.Role, &tk, &e.To.Role, &actor, &e.Reason, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan crew baton event: %w", err)
		}
		e.From.Kind, e.To.Kind, e.Actor = domain.BatonHolderKind(fk), domain.BatonHolderKind(tk), domain.BatonActor(actor)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolveNudgeTarget says where automatic work for a task's worker goes. The
// baton holder receives it: the worker itself, or the holding role's session.
// deliver is false while the human holds the baton or the holder cannot be
// reached; nothing is lost, because an undelivered nudge is not recorded as sent
// and fires once the baton moves.
func (s *Store) ResolveNudgeTarget(ctx context.Context, owner domain.SessionID) (target domain.SessionID, deliver bool, err error) {
	holder, err := effectiveBaton(ctx, s.readDB, owner)
	if err != nil {
		return "", false, err
	}
	switch holder.Kind {
	case domain.BatonHolderHuman:
		return "", false, nil
	case domain.BatonHolderRole:
		id, live, err := liveRoleSession(ctx, s.readDB, owner, holder.Role)
		return id, live, err
	}
	return owner, true, nil
}
