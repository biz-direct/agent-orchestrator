package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// These DTOs mirror the daemon's crew catalog API without importing
// controller or service types.
type crewDiagnosticDTO struct {
	File    string `json:"file,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type crewRoleDTO struct {
	ID                 string `json:"id"`
	Description        string `json:"description"`
	InstructionsSource string `json:"instructionsSource"`
	Harness            string `json:"harness,omitempty"`
	Model              string `json:"model,omitempty"`
}

type crewRoleEntryDTO struct {
	ID          string              `json:"id"`
	File        string              `json:"file"`
	Valid       bool                `json:"valid"`
	Role        *crewRoleDTO        `json:"role,omitempty"`
	Diagnostics []crewDiagnosticDTO `json:"diagnostics"`
}

type crewReturnDTO struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

type crewWorkflowBodyDTO struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Roles       []string        `json:"roles"`
	Order       []string        `json:"order"`
	Returns     []crewReturnDTO `json:"returns"`
	LoopCap     int             `json:"loopCap"`
}

type crewWorkflowEntryDTO struct {
	ID          string               `json:"id"`
	File        string               `json:"file"`
	Valid       bool                 `json:"valid"`
	Workflow    *crewWorkflowBodyDTO `json:"workflow,omitempty"`
	Diagnostics []crewDiagnosticDTO  `json:"diagnostics"`
}

type crewSelectionDTO struct {
	Mode       string `json:"mode"`
	WorkflowID string `json:"workflowId,omitempty"`
}

type crewDefaultDTO struct {
	Selection *crewSelectionDTO `json:"selection"`
	State     string            `json:"state"`
	Message   string            `json:"message"`
}

type crewCatalogDTO struct {
	ProjectID   string                 `json:"projectId"`
	Roles       []crewRoleEntryDTO     `json:"roles"`
	Workflows   []crewWorkflowEntryDTO `json:"workflows"`
	Diagnostics []crewDiagnosticDTO    `json:"diagnostics"`
	Default     crewDefaultDTO         `json:"default"`
}

type crewSetDefaultDTO struct {
	Selection *crewSelectionDTO `json:"selection"`
}

const crewProjectFlagHelp = "Project id (default: AO_PROJECT_ID, current session, or current registered repo)"

func newCrewCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "crew",
		Short: "Inspect repository-defined crew roles and workflows and the project default",
		Long: "Crew roles and workflows are defined in repository files under .ao/crew/roles and " +
			".ao/crew/workflows. AO discovers and validates them; there is no editor.",
	}
	cmd.AddCommand(
		newCrewListCommand(ctx),
		newCrewValidateCommand(ctx),
		newCrewDefaultCommand(ctx),
	)
	return cmd
}

func (c *commandContext) fetchCrewCatalog(cmd *cobra.Command, project string) (crewCatalogDTO, error) {
	resolved, err := c.resolveSpawnProject(cmd.Context(), project)
	if err != nil {
		return crewCatalogDTO{}, err
	}
	var res crewCatalogDTO
	if err := c.getJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/crew", &res); err != nil {
		return crewCatalogDTO{}, err
	}
	return res, nil
}

func newCrewListCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List discovered roles and workflows with validation status and the project default",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := ctx.fetchCrewCatalog(cmd, project)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeCrewCatalog(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", crewProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON, including every diagnostic")
	return cmd
}

func newCrewValidateCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate repository crew definitions; exits 1 when any are invalid",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := ctx.fetchCrewCatalog(cmd, project)
			if err != nil {
				return err
			}
			problems := countCrewProblems(res)
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else if err := writeCrewDiagnostics(cmd.OutOrStdout(), res); err != nil {
				return err
			}
			if problems > 0 {
				return fmt.Errorf("%d crew definition problem(s) found", problems)
			}
			if !jsonOutput {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %d role(s), %d workflow(s) valid\n", len(res.Roles), len(res.Workflows))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&project, "project", "", crewProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newCrewDefaultCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default",
		Short: "Read or change the project-default crew workflow",
	}
	cmd.AddCommand(newCrewDefaultGetCommand(ctx), newCrewDefaultSetCommand(ctx), newCrewDefaultClearCommand(ctx))
	return cmd
}

func newCrewDefaultGetCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Show the project-default crew selection and whether it can be used",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := ctx.resolveSpawnProject(cmd.Context(), project)
			if err != nil {
				return err
			}
			var res crewDefaultDTO
			if err := ctx.getJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/crew/default", &res); err != nil {
				return err
			}
			return writeCrewDefault(cmd.OutOrStdout(), res, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", crewProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newCrewDefaultSetCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput, noCrew bool
	cmd := &cobra.Command{
		Use:   "set [workflow-id]",
		Short: "Save the project-default crew workflow (or --no-crew) without changing other settings",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			workflow := ""
			if len(args) == 1 {
				workflow = strings.TrimSpace(args[0])
			}
			if noCrew == (workflow != "") {
				return usageError{errors.New("provide exactly one of a workflow id or --no-crew")}
			}
			sel := &crewSelectionDTO{Mode: "no_crew"}
			if workflow != "" {
				sel = &crewSelectionDTO{Mode: "workflow", WorkflowID: workflow}
			}
			return ctx.putCrewDefault(cmd, project, sel, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", crewProjectFlagHelp)
	cmd.Flags().BoolVar(&noCrew, "no-crew", false, "Explicitly choose tasks without a crew instead of a workflow")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newCrewDefaultClearCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove the project-default crew selection",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.putCrewDefault(cmd, project, nil, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", crewProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func (c *commandContext) putCrewDefault(cmd *cobra.Command, project string, sel *crewSelectionDTO, jsonOutput bool) error {
	resolved, err := c.resolveSpawnProject(cmd.Context(), project)
	if err != nil {
		return err
	}
	var res crewDefaultDTO
	if err := c.putJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/crew/default", crewSetDefaultDTO{Selection: sel}, &res); err != nil {
		return err
	}
	return writeCrewDefault(cmd.OutOrStdout(), res, jsonOutput)
}

func writeCrewDefault(w io.Writer, res crewDefaultDTO, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(w, res)
	}
	_, err := fmt.Fprintf(w, "default: %s\n%s\n", describeCrewSelection(res.Selection), res.Message)
	return err
}

func describeCrewSelection(sel *crewSelectionDTO) string {
	switch {
	case sel == nil:
		return "(none)"
	case sel.Mode == "workflow":
		return "workflow " + sel.WorkflowID
	default:
		return "no crew"
	}
}

func countCrewProblems(res crewCatalogDTO) int {
	n := len(res.Diagnostics)
	for _, r := range res.Roles {
		n += len(r.Diagnostics)
	}
	for _, w := range res.Workflows {
		n += len(w.Diagnostics)
	}
	return n
}

func writeCrewDiagnostics(w io.Writer, res crewCatalogDTO) error {
	write := func(ds []crewDiagnosticDTO) {
		for _, d := range ds {
			where := d.File
			if d.Field != "" {
				where += " [" + d.Field + "]"
			}
			if where == "" {
				_, _ = fmt.Fprintf(w, "error: %s\n", d.Message)
				continue
			}
			_, _ = fmt.Fprintf(w, "error: %s: %s\n", where, d.Message)
		}
	}
	write(res.Diagnostics)
	for _, r := range res.Roles {
		write(r.Diagnostics)
	}
	for _, wf := range res.Workflows {
		write(wf.Diagnostics)
	}
	return nil
}

func writeCrewCatalog(w io.Writer, res crewCatalogDTO) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tID\tSTATUS\tDETAIL")
	for _, r := range res.Roles {
		status, detail := "valid", ""
		if r.Role != nil {
			detail = r.Role.Description
		}
		if !r.Valid {
			status, detail = "invalid", fmt.Sprintf("%d problem(s) in %s", len(r.Diagnostics), r.File)
		}
		_, _ = fmt.Fprintf(tw, "role\t%s\t%s\t%s\n", r.ID, status, detail)
	}
	for _, wf := range res.Workflows {
		status, detail := "valid", ""
		if wf.Workflow != nil {
			detail = strings.Join(wf.Workflow.Order, " → ")
		}
		if !wf.Valid {
			status, detail = "invalid", fmt.Sprintf("%d problem(s) in %s", len(wf.Diagnostics), wf.File)
		}
		_, _ = fmt.Fprintf(tw, "workflow\t%s\t%s\t%s\n", wf.ID, status, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\ndefault: %s (%s)\n", describeCrewSelection(res.Default.Selection), res.Default.State); err != nil {
		return err
	}
	if countCrewProblems(res) > 0 {
		_, err := fmt.Fprintln(w, "Run `ao crew validate` for the full list of problems.")
		return err
	}
	return nil
}
