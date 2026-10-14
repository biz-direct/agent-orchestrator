package domain

import (
	"errors"
	"strings"
	"time"
)

// The crew write baton. Exactly one holder per task may change code; everyone
// else is read-only. A task with no recorded baton is held by its worker.

// BatonHolderKind says what kind of party holds the baton.
type BatonHolderKind string

const (
	BatonHolderWorker BatonHolderKind = "worker"
	BatonHolderRole   BatonHolderKind = "role"
	BatonHolderHuman  BatonHolderKind = "human"
)

// BatonHolder is the single party that may write.
type BatonHolder struct {
	Kind BatonHolderKind `json:"kind" enum:"worker,role,human"`
	// Role is the role id, set only when Kind is role.
	Role string `json:"role,omitempty"`
}

// Validate checks the kind/role pairing.
func (h BatonHolder) Validate() error {
	switch h.Kind {
	case BatonHolderWorker, BatonHolderHuman:
		if strings.TrimSpace(h.Role) != "" {
			return errors.New("only a role holder carries a role id")
		}
	case BatonHolderRole:
		if strings.TrimSpace(h.Role) == "" {
			return errors.New("a role holder needs a role id")
		}
	default:
		return errors.New("holder kind must be worker, role, or human")
	}
	return nil
}

// String renders the holder for prompts, history, and CLI output.
func (h BatonHolder) String() string {
	if h.Kind == BatonHolderRole {
		return "role " + h.Role
	}
	return string(h.Kind)
}

// BatonActor says who moved the baton.
type BatonActor string

const (
	BatonActorHuman  BatonActor = "human"
	BatonActorSystem BatonActor = "system"
)

// BatonEvent is one recorded baton move.
type BatonEvent struct {
	ID        int64       `json:"id"`
	From      BatonHolder `json:"from"`
	To        BatonHolder `json:"to"`
	Actor     BatonActor  `json:"actor" enum:"human,system"`
	Reason    string      `json:"reason"`
	CreatedAt time.Time   `json:"createdAt"`
}

// ErrBatonRoleNotLive means the target role has no live session on the task.
var ErrBatonRoleNotLive = errors.New("the task has no live session for that role")
