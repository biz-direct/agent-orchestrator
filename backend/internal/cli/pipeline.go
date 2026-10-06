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

// These DTOs mirror the daemon's pipeline catalog API without importing
// controller or service types.
type pipelineDiagnosticDTO struct {
	File    string `json:"file,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type pipelineProfileDTO struct {
	ID                 string `json:"id"`
	Description        string `json:"description"`
	InstructionsSource string `json:"instructionsSource"`
	Harness            string `json:"harness,omitempty"`
	Model              string `json:"model,omitempty"`
}

type pipelineProfileEntryDTO struct {
	ID          string                  `json:"id"`
	File        string                  `json:"file"`
	Valid       bool                    `json:"valid"`
	Profile     *pipelineProfileDTO     `json:"profile,omitempty"`
	Diagnostics []pipelineDiagnosticDTO `json:"diagnostics"`
}

type pipelineStageDTO struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Profile  string `json:"profile,omitempty"`
	RepairTo string `json:"repairTo,omitempty"`
}

type pipelineWorkflowBodyDTO struct {
	ID           string             `json:"id"`
	Description  string             `json:"description"`
	RepairBudget int                `json:"repairBudget"`
	Stages       []pipelineStageDTO `json:"stages"`
}

type pipelineWorkflowDTO struct {
	ID                string                   `json:"id"`
	File              string                   `json:"file"`
	Valid             bool                     `json:"valid"`
	Workflow          *pipelineWorkflowBodyDTO `json:"workflow,omitempty"`
	Diagnostics       []pipelineDiagnosticDTO  `json:"diagnostics"`
	Executable        bool                     `json:"executable"`
	UnavailableReason string                   `json:"unavailableReason,omitempty"`
}

type pipelineSelectionDTO struct {
	Mode       string `json:"mode"`
	WorkflowID string `json:"workflowId,omitempty"`
}

type pipelineDefaultDTO struct {
	Selection  *pipelineSelectionDTO `json:"selection"`
	State      string                `json:"state"`
	Executable bool                  `json:"executable"`
	Message    string                `json:"message"`
}

type pipelineCatalogDTO struct {
	ProjectID   string                    `json:"projectId"`
	Profiles    []pipelineProfileEntryDTO `json:"profiles"`
	Workflows   []pipelineWorkflowDTO     `json:"workflows"`
	Diagnostics []pipelineDiagnosticDTO   `json:"diagnostics"`
	Default     pipelineDefaultDTO        `json:"default"`
}

type pipelineSetDefaultDTO struct {
	Selection *pipelineSelectionDTO `json:"selection"`
}

func newPipelineCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pipeline",
		Aliases: []string{"pipelines"},
		Short:   "Inspect repository-defined specialist pipelines and the project default",
		Long: "Pipelines are defined in repository files under .ao/pipelines/profiles and " +
			".ao/pipelines/workflows. AO discovers and validates them; there is no editor. " +
			"A valid workflow that this build cannot execute yet is shown as unavailable and " +
			"is never silently replaced by a normal worker.",
	}
	cmd.AddCommand(
		newPipelineListCommand(ctx),
		newPipelineValidateCommand(ctx),
		newPipelineDefaultCommand(ctx),
	)
	return cmd
}

const pipelineProjectFlagHelp = "Project id (default: AO_PROJECT_ID, current session, or current registered repo)"

func (c *commandContext) fetchPipelineCatalog(cmd *cobra.Command, project string) (pipelineCatalogDTO, error) {
	resolved, err := c.resolveSpawnProject(cmd.Context(), project)
	if err != nil {
		return pipelineCatalogDTO{}, err
	}
	var res pipelineCatalogDTO
	if err := c.getJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/pipelines", &res); err != nil {
		return pipelineCatalogDTO{}, err
	}
	return res, nil
}

func newPipelineListCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List discovered profiles and workflows with validation status",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := ctx.fetchPipelineCatalog(cmd, project)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writePipelineCatalog(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", pipelineProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON, including every diagnostic")
	return cmd
}

func newPipelineValidateCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate repository pipeline definitions; exits 1 when any are invalid",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := ctx.fetchPipelineCatalog(cmd, project)
			if err != nil {
				return err
			}
			invalid := countPipelineProblems(res)
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else if err := writePipelineDiagnostics(cmd.OutOrStdout(), res); err != nil {
				return err
			}
			if invalid > 0 {
				return fmt.Errorf("%d pipeline definition problem(s) found", invalid)
			}
			if !jsonOutput {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %d profile(s), %d workflow(s) valid\n", len(res.Profiles), len(res.Workflows))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&project, "project", "", pipelineProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newPipelineDefaultCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default",
		Short: "Read or change the project-default pipeline",
	}
	cmd.AddCommand(newPipelineDefaultGetCommand(ctx), newPipelineDefaultSetCommand(ctx), newPipelineDefaultClearCommand(ctx))
	return cmd
}

func newPipelineDefaultGetCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Show the project-default pipeline selection and whether it can run",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := ctx.resolveSpawnProject(cmd.Context(), project)
			if err != nil {
				return err
			}
			var res pipelineDefaultDTO
			if err := ctx.getJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/pipelines/default", &res); err != nil {
				return err
			}
			return writePipelineDefault(cmd.OutOrStdout(), res, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", pipelineProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newPipelineDefaultSetCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput, normalWorker bool
	cmd := &cobra.Command{
		Use:   "set [workflow-id]",
		Short: "Save the project-default pipeline (or --normal-worker) without changing other settings",
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
			if normalWorker == (workflow != "") {
				return usageError{errors.New("provide exactly one of a workflow id or --normal-worker")}
			}
			sel := &pipelineSelectionDTO{Mode: "normal_worker"}
			if workflow != "" {
				sel = &pipelineSelectionDTO{Mode: "workflow", WorkflowID: workflow}
			}
			return ctx.putPipelineDefault(cmd, project, sel, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", pipelineProjectFlagHelp)
	cmd.Flags().BoolVar(&normalWorker, "normal-worker", false, "Explicitly choose the ordinary single worker instead of a workflow")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newPipelineDefaultClearCommand(ctx *commandContext) *cobra.Command {
	var project string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove the project-default pipeline selection",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.putPipelineDefault(cmd, project, nil, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", pipelineProjectFlagHelp)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func (c *commandContext) putPipelineDefault(cmd *cobra.Command, project string, sel *pipelineSelectionDTO, jsonOutput bool) error {
	resolved, err := c.resolveSpawnProject(cmd.Context(), project)
	if err != nil {
		return err
	}
	var res pipelineDefaultDTO
	if err := c.putJSON(cmd.Context(), "projects/"+url.PathEscape(resolved.ID)+"/pipelines/default", pipelineSetDefaultDTO{Selection: sel}, &res); err != nil {
		return err
	}
	return writePipelineDefault(cmd.OutOrStdout(), res, jsonOutput)
}

func writePipelineDefault(w io.Writer, res pipelineDefaultDTO, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(w, res)
	}
	_, err := fmt.Fprintf(w, "default: %s\n%s\n", describePipelineSelection(res.Selection), res.Message)
	return err
}

func describePipelineSelection(sel *pipelineSelectionDTO) string {
	switch {
	case sel == nil:
		return "(none)"
	case sel.Mode == "workflow":
		return "workflow " + sel.WorkflowID
	default:
		return "normal worker"
	}
}

func countPipelineProblems(res pipelineCatalogDTO) int {
	n := len(res.Diagnostics)
	for _, p := range res.Profiles {
		n += len(p.Diagnostics)
	}
	for _, w := range res.Workflows {
		n += len(w.Diagnostics)
	}
	return n
}

func writePipelineDiagnostics(w io.Writer, res pipelineCatalogDTO) error {
	write := func(ds []pipelineDiagnosticDTO) {
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
	for _, p := range res.Profiles {
		write(p.Diagnostics)
	}
	for _, wf := range res.Workflows {
		write(wf.Diagnostics)
	}
	return nil
}

func writePipelineCatalog(w io.Writer, res pipelineCatalogDTO) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tID\tSTATUS\tDETAIL")
	for _, p := range res.Profiles {
		status, detail := "valid", ""
		if p.Profile != nil {
			detail = p.Profile.Description
		}
		if !p.Valid {
			status, detail = "invalid", fmt.Sprintf("%d problem(s) in %s", len(p.Diagnostics), p.File)
		}
		_, _ = fmt.Fprintf(tw, "profile\t%s\t%s\t%s\n", p.ID, status, detail)
	}
	for _, wf := range res.Workflows {
		status, detail := "unavailable", wf.UnavailableReason
		switch {
		case !wf.Valid:
			status, detail = "invalid", fmt.Sprintf("%d problem(s) in %s", len(wf.Diagnostics), wf.File)
		case wf.Executable:
			status, detail = "available", ""
			if wf.Workflow != nil {
				detail = wf.Workflow.Description
			}
		}
		_, _ = fmt.Fprintf(tw, "workflow\t%s\t%s\t%s\n", wf.ID, status, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\ndefault: %s (%s)\n", describePipelineSelection(res.Default.Selection), res.Default.State); err != nil {
		return err
	}
	if countPipelineProblems(res) > 0 {
		_, err := fmt.Fprintln(w, "Run `ao pipeline validate` for the full list of problems.")
		return err
	}
	return nil
}
