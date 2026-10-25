package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// These DTOs mirror the daemon's crew baton API.
type crewBatonHolderDTO struct {
	Kind string `json:"kind"`
	Role string `json:"role,omitempty"`
}

type crewBatonEventDTO struct {
	From      crewBatonHolderDTO `json:"from"`
	To        crewBatonHolderDTO `json:"to"`
	Actor     string             `json:"actor"`
	Reason    string             `json:"reason"`
	CreatedAt time.Time          `json:"createdAt"`
}

type crewBatonStatusDTO struct {
	OwnerSessionID string              `json:"ownerSessionId"`
	Holder         crewBatonHolderDTO  `json:"holder"`
	History        []crewBatonEventDTO `json:"history"`
}

type crewMoveBatonDTO struct {
	To     crewBatonHolderDTO `json:"to"`
	Reason string             `json:"reason,omitempty"`
}

func (h crewBatonHolderDTO) String() string {
	if h.Kind == "role" {
		return "role " + h.Role
	}
	return h.Kind
}

// parseBatonTarget reads "worker", "human", or a role id.
func parseBatonTarget(arg string) crewBatonHolderDTO {
	switch target := strings.TrimSpace(arg); target {
	case "worker", "human":
		return crewBatonHolderDTO{Kind: target}
	default:
		return crewBatonHolderDTO{Kind: "role", Role: target}
	}
}

func newCrewStatusCommand(ctx *commandContext) *cobra.Command {
	var session string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the task's roles, who holds the write baton, and every baton move",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := pipelineSessionID(session)
			if err != nil {
				return err
			}
			var baton crewBatonStatusDTO
			if err := ctx.getJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/crew/baton", &baton); err != nil {
				return err
			}
			var roles crewRoleListDTO
			if err := ctx.getJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/crew/roles", &roles); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), struct {
					Baton crewBatonStatusDTO   `json:"baton"`
					Roles []crewRoleSessionDTO `json:"roles"`
				}{baton, roles.Roles})
			}
			return writeCrewStatus(cmd.OutOrStdout(), baton, roles.Roles)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", crewSessionFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func writeCrewStatus(w io.Writer, baton crewBatonStatusDTO, roles []crewRoleSessionDTO) error {
	if _, err := fmt.Fprintf(w, "write baton: %s\n\n", baton.Holder); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ROLE\tSESSION\tHARNESS\tACCESS")
	access := func(held bool) string {
		if held {
			return "writes (baton)"
		}
		return "read-only"
	}
	_, _ = fmt.Fprintf(tw, "worker\t%s\t\t%s\n", baton.OwnerSessionID, access(baton.Holder.Kind == "worker"))
	for _, r := range roles {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Role, r.SessionID, r.Harness, access(baton.Holder.Kind == "role" && baton.Holder.Role == r.Role))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(baton.History) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nbaton history:"); err != nil {
		return err
	}
	for _, e := range baton.History {
		line := fmt.Sprintf("  %s  %s -> %s  by %s", e.CreatedAt.Local().Format("2006-01-02 15:04:05"), e.From, e.To, e.Actor)
		if e.Reason != "" {
			line += ": " + e.Reason
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func newCrewBatonCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "baton",
		Short: "Move the write baton (human only)",
		Long: "Exactly one party holds the write baton and may change code: the worker, one role, or the human. " +
			"Everyone else is read-only. Only a person can move it; orchestrators and roles cannot.",
	}
	cmd.AddCommand(newCrewBatonGiveCommand(ctx), newCrewBatonTakeCommand(ctx))
	return cmd
}

func newCrewBatonGiveCommand(ctx *commandContext) *cobra.Command {
	var session, reason string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "give <role|worker|human>",
		Short: "Give the write baton to a live role, the worker, or the human",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.moveCrewBaton(cmd, session, parseBatonTarget(args[0]), reason, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", crewSessionFlagHelp)
	cmd.Flags().StringVar(&reason, "reason", "", "Why the baton is moving (recorded in the crew history)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newCrewBatonTakeCommand(ctx *commandContext) *cobra.Command {
	var session, reason string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "take",
		Short: "Take the write baton yourself; every agent becomes read-only",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.moveCrewBaton(cmd, session, crewBatonHolderDTO{Kind: "human"}, reason, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", crewSessionFlagHelp)
	cmd.Flags().StringVar(&reason, "reason", "", "Why the baton is moving (recorded in the crew history)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func (c *commandContext) moveCrewBaton(cmd *cobra.Command, session string, to crewBatonHolderDTO, reason string, jsonOutput bool) error {
	id, err := pipelineSessionID(session)
	if err != nil {
		return err
	}
	var res crewBatonStatusDTO
	if err := c.doJSONPathWithHeaders(cmd.Context(), http.MethodPut, "/api/v1/sessions/"+url.PathEscape(id)+"/crew/baton",
		crewMoveBatonDTO{To: to, Reason: strings.TrimSpace(reason)}, &res, pipelineCallerHeaders()); err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "write baton for %s: %s\n", id, res.Holder)
	return err
}
