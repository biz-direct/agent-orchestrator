package crew

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// unsupportedGraphKeys are top-level workflow fields that would turn the
// baton policy into a parallel or graph engine. They get a dedicated message
// instead of a generic unknown-field error.
var unsupportedGraphKeys = map[string]bool{
	"parallel": true, "dependsOn": true, "depends_on": true, "needs": true,
	"next": true, "branches": true, "when": true, "if": true, "matrix": true,
	"fanOut": true, "fanIn": true, "after": true, "on": true, "graph": true,
	"nodes": true, "edges": true, "stages": true, "steps": true, "jobs": true,
}

// Discover reads every role and workflow definition under root and returns
// the validated catalog. It never returns an error: problems become
// diagnostics so the desktop can show them next to the definition.
func Discover(root string) Catalog {
	cat := Catalog{
		Roles:       []RoleEntry{},
		Workflows:   []WorkflowEntry{},
		Diagnostics: []Diagnostic{},
	}
	if strings.TrimSpace(root) == "" {
		cat.Diagnostics = append(cat.Diagnostics, Diagnostic{Message: "project path is empty"})
		return cat
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		cat.Diagnostics = append(cat.Diagnostics, Diagnostic{Message: fmt.Sprintf("project folder is not readable: %v", err)})
		return cat
	}
	d := &discoverer{root: root, resolvedRoot: resolvedRoot}

	roleFiles, diag := d.listDefinitionFiles(RolesDir)
	cat.Diagnostics = append(cat.Diagnostics, diag...)
	for _, f := range roleFiles {
		cat.Roles = append(cat.Roles, d.loadRole(f))
	}
	workflowFiles, diag := d.listDefinitionFiles(WorkflowsDir)
	cat.Diagnostics = append(cat.Diagnostics, diag...)
	for _, f := range workflowFiles {
		cat.Workflows = append(cat.Workflows, d.loadWorkflow(f))
	}

	flagDuplicateRoles(cat.Roles)
	flagDuplicateWorkflows(cat.Workflows)
	crossValidateWorkflows(cat)

	for i := range cat.Roles {
		cat.Roles[i].Valid = len(cat.Roles[i].Diagnostics) == 0
		if !cat.Roles[i].Valid {
			cat.Roles[i].Role = nil
		}
	}
	for i := range cat.Workflows {
		cat.Workflows[i].Valid = len(cat.Workflows[i].Diagnostics) == 0
		if !cat.Workflows[i].Valid {
			cat.Workflows[i].Workflow = nil
		}
	}
	return cat
}

type discoverer struct {
	root         string
	resolvedRoot string
}

func (d *discoverer) listDefinitionFiles(dir string) ([]string, []Diagnostic) {
	abs, err := d.safePath(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []Diagnostic{{File: dir, Message: err.Error()}}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []Diagnostic{{File: dir, Message: fmt.Sprintf("cannot read directory: %v", err)}}
	}
	var files []string
	var diags []Diagnostic
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, dir+"/"+e.Name())
	}
	sort.Strings(files)
	if len(files) > MaxDefinitionsPerKind {
		diags = append(diags, Diagnostic{File: dir, Message: fmt.Sprintf("too many definition files (%d); at most %d are read", len(files), MaxDefinitionsPerKind)})
		files = files[:MaxDefinitionsPerKind]
	}
	return files, diags
}

// safePath resolves a repo-relative path and proves the result stays inside
// the project root even through symlinked parents.
func (d *discoverer) safePath(rel string) (string, error) {
	if err := validateRepoRelativePath(rel); err != nil {
		return "", err
	}
	abs := filepath.Join(d.root, filepath.FromSlash(rel))
	// Reject symlinks at every component below the registered root, even when
	// the target stays inside the repository. A symlinked registered root is
	// intentional and handled separately through resolvedRoot.
	cur := d.root
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s passes through a symlink (%s); symlinked definition and instruction paths are not supported", rel, seg)
		}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	prefix := d.resolvedRoot + string(filepath.Separator)
	if resolved != d.resolvedRoot && !strings.HasPrefix(resolved, prefix) {
		return "", fmt.Errorf("%s resolves outside the project folder", rel)
	}
	return abs, nil
}

// readRegularFile reads a size-bounded regular, non-symlink file.
func (d *discoverer) readRegularFile(rel string, limit int64) ([]byte, error) {
	abs, err := d.safePath(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist", rel)
		}
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; symlinked definition and instruction files are not supported", rel)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is too large (%d bytes; limit %d)", rel, info.Size(), limit)
	}
	data, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the project root by safePath
	if err != nil {
		return nil, err
	}
	return data, nil
}

// --- raw (decoded) shapes -------------------------------------------------

type rawCommand struct {
	ID             string `yaml:"id"`
	Command        string `yaml:"command"`
	TimeoutSeconds *int   `yaml:"timeoutSeconds"`
	Required       *bool  `yaml:"required"`
}

type rawRole struct {
	Version          *int         `yaml:"version"`
	ID               string       `yaml:"id"`
	Description      string       `yaml:"description"`
	Instructions     string       `yaml:"instructions"`
	InstructionsFile string       `yaml:"instructionsFile"`
	Harness          string       `yaml:"harness"`
	Model            string       `yaml:"model"`
	AllowedPaths     []string     `yaml:"allowedPaths"`
	Validation       []rawCommand `yaml:"validation"`
}

type rawWorkflow struct {
	Version          *int                `yaml:"version"`
	ID               string              `yaml:"id"`
	Description      string              `yaml:"description"`
	Instructions     string              `yaml:"instructions"`
	InstructionsFile string              `yaml:"instructionsFile"`
	Roles            []string            `yaml:"roles"`
	Order            []string            `yaml:"order"`
	Returns          map[string][]string `yaml:"returns"`
	LoopCap          *int                `yaml:"loopCap"`
}

// decodeStrict parses one definition document with duplicate-key, unknown-field,
// alias, and multi-document rejection. Unsupported graph keys are reported
// first because they are the likeliest authoring mistake.
func decodeStrict(data []byte, out any, file string) []Diagnostic {
	if !utf8.Valid(data) {
		return []Diagnostic{{File: file, Message: "file is not valid UTF-8"}}
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return []Diagnostic{{File: file, Message: yamlMessage(err)}}
	}
	if len(node.Content) == 0 {
		return []Diagnostic{{File: file, Message: "file is empty"}}
	}
	var diags []Diagnostic
	walkNode(&node, "", file, &diags)
	if len(diags) > 0 {
		return diags
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			for _, m := range typeErr.Errors {
				diags = append(diags, Diagnostic{File: file, Message: rewriteYAMLMessage(m)})
			}
			return diags
		}
		return []Diagnostic{{File: file, Message: yamlMessage(err)}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return []Diagnostic{{File: file, Message: "multiple YAML documents in one file are ambiguous; use one definition per file"}}
	}
	return nil
}

func walkNode(n *yaml.Node, field, file string, diags *[]Diagnostic) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		*diags = append(*diags, Diagnostic{File: file, Field: field, Message: fmt.Sprintf("line %d: YAML anchors and aliases are not supported because they make definitions ambiguous", n.Line)})
		return
	}
	if n.Tag == "!!merge" {
		*diags = append(*diags, Diagnostic{File: file, Field: field, Message: fmt.Sprintf("line %d: YAML merge keys are not supported because they make definitions ambiguous", n.Line)})
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, c := range n.Content {
			sub := field
			if n.Kind == yaml.SequenceNode {
				sub = fmt.Sprintf("%s[%d]", field, i)
			}
			walkNode(c, sub, file, diags)
		}
	case yaml.MappingNode:
		topLevel := field == ""
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			walkNode(key, field, file, diags)
			sub := key.Value
			if field != "" {
				sub = field + "." + key.Value
			}
			if topLevel && isWorkflowFile(file) && unsupportedGraphKeys[key.Value] {
				*diags = append(*diags, Diagnostic{File: file, Field: sub, Message: fmt.Sprintf("line %d: %q is a parallel or graph feature; crew workflows are a single forward order with returns to earlier roles (no parallel, branching, graph, or dependency features)", key.Line, key.Value)})
				continue
			}
			walkNode(n.Content[i+1], sub, file, diags)
		}
	}
}

var unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)

func rewriteYAMLMessage(m string) string {
	if sub := unknownFieldRE.FindStringSubmatch(m); sub != nil {
		return fmt.Sprintf("line %s: unknown field %q", sub[1], sub[2])
	}
	return strings.TrimPrefix(m, "yaml: ")
}

func yamlMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

// --- role --------------------------------------------------------------

func (d *discoverer) loadRole(file string) RoleEntry {
	entry := RoleEntry{ID: fileStem(file), File: file, Diagnostics: []Diagnostic{}}
	data, err := d.readRegularFile(file, MaxDefinitionBytes)
	if err != nil {
		entry.Diagnostics = append(entry.Diagnostics, Diagnostic{File: file, Message: err.Error()})
		return entry
	}
	var raw rawRole
	if diags := decodeStrict(data, &raw, file); len(diags) > 0 {
		entry.Diagnostics = diags
		return entry
	}
	if raw.ID != "" {
		entry.ID = raw.ID
	}
	add := func(field, format string, args ...any) {
		entry.Diagnostics = append(entry.Diagnostics, Diagnostic{File: file, Field: field, Message: fmt.Sprintf(format, args...)})
	}
	checkVersion(raw.Version, add)
	checkID(raw.ID, "role", add)
	if raw.ID == ReviewStep {
		add("id", "%q is reserved for the review step and cannot be a role id", ReviewStep)
	}

	p := Role{
		ID:               raw.ID,
		DefinitionSHA256: sha256Hex(data),
		Description:      strings.TrimSpace(raw.Description),
		Harness:          strings.TrimSpace(raw.Harness),
		Model:            strings.TrimSpace(raw.Model),
	}
	if p.Description == "" {
		add("description", "description is required so users can tell roles apart")
	}
	if p.Harness != "" && !domain.AgentHarness(p.Harness).IsKnown() {
		add("harness", "unknown harness %q", p.Harness)
	}
	if raw.Instructions == "" && raw.InstructionsFile == "" {
		add("instructions", "specify exactly one of instructions or instructionsFile")
	} else if text, source, sum, ok := d.resolveInstructions(raw.Instructions, raw.InstructionsFile, add); ok {
		p.InstructionsText, p.InstructionsSource, p.InstructionsSHA256 = text, source, sum
	}

	p.AllowedPaths = []string{}
	if len(raw.AllowedPaths) > MaxAllowedPaths {
		add("allowedPaths", "too many allowed paths (%d); at most %d", len(raw.AllowedPaths), MaxAllowedPaths)
	}
	seenPath := map[string]bool{}
	for i, g := range raw.AllowedPaths {
		field := fmt.Sprintf("allowedPaths[%d]", i)
		if err := validateGlob(g); err != nil {
			add(field, "%v", err)
			continue
		}
		if seenPath[g] {
			add(field, "duplicate allowed path %q", g)
		}
		seenPath[g] = true
		p.AllowedPaths = append(p.AllowedPaths, g)
	}

	p.Validation = []ValidationCommand{}
	if len(raw.Validation) > MaxValidationCommands {
		add("validation", "too many validation commands (%d); at most %d", len(raw.Validation), MaxValidationCommands)
	}
	seenCmd := map[string]bool{}
	for i, c := range raw.Validation {
		field := fmt.Sprintf("validation[%d]", i)
		cmd := ValidationCommand{ID: c.ID, Command: strings.TrimSpace(c.Command), TimeoutSeconds: DefaultValidationTimeoutSeconds, Required: true}
		if !idPattern.MatchString(c.ID) {
			add(field+".id", "id %q must be lowercase letters, digits, and hyphens", c.ID)
		} else if seenCmd[c.ID] {
			add(field+".id", "duplicate validation command id %q", c.ID)
		}
		seenCmd[c.ID] = true
		switch {
		case cmd.Command == "":
			add(field+".command", "command is required")
		case len(cmd.Command) > MaxCommandBytes:
			add(field+".command", "command is too long (limit %d bytes)", MaxCommandBytes)
		case strings.ContainsRune(cmd.Command, 0):
			add(field+".command", "command must not contain NUL bytes")
		}
		if c.TimeoutSeconds != nil {
			if *c.TimeoutSeconds < 1 || *c.TimeoutSeconds > MaxValidationTimeoutSeconds {
				add(field+".timeoutSeconds", "timeoutSeconds must be between 1 and %d", MaxValidationTimeoutSeconds)
			} else {
				cmd.TimeoutSeconds = *c.TimeoutSeconds
			}
		}
		if c.Required != nil {
			cmd.Required = *c.Required
		}
		p.Validation = append(p.Validation, cmd)
	}
	entry.Role = &p
	return entry
}

// --- workflow -------------------------------------------------------------

func isWorkflowFile(file string) bool {
	return strings.HasPrefix(file, WorkflowsDir+"/")
}

func (d *discoverer) loadWorkflow(file string) WorkflowEntry {
	entry := WorkflowEntry{ID: fileStem(file), File: file, Diagnostics: []Diagnostic{}}
	data, err := d.readRegularFile(file, MaxDefinitionBytes)
	if err != nil {
		entry.Diagnostics = append(entry.Diagnostics, Diagnostic{File: file, Message: err.Error()})
		return entry
	}
	var raw rawWorkflow
	if diags := decodeStrict(data, &raw, file); len(diags) > 0 {
		entry.Diagnostics = diags
		return entry
	}
	if raw.ID != "" {
		entry.ID = raw.ID
	}
	add := func(field, format string, args ...any) {
		entry.Diagnostics = append(entry.Diagnostics, Diagnostic{File: file, Field: field, Message: fmt.Sprintf(format, args...)})
	}
	checkVersion(raw.Version, add)
	checkID(raw.ID, "workflow", add)

	w := Workflow{ID: raw.ID, DefinitionSHA256: sha256Hex(data), Description: strings.TrimSpace(raw.Description), LoopCap: DefaultLoopCap}
	if w.Description == "" {
		add("description", "description is required so users can tell workflows apart")
	}
	if raw.Instructions != "" || raw.InstructionsFile != "" {
		if text, source, sum, ok := d.resolveInstructions(raw.Instructions, raw.InstructionsFile, add); ok {
			w.InstructionsText, w.InstructionsSource, w.InstructionsSHA256 = text, source, sum
		}
	}
	if raw.LoopCap != nil {
		if *raw.LoopCap < 1 || *raw.LoopCap > MaxLoopCap {
			add("loopCap", "loopCap must be between 1 and %d", MaxLoopCap)
		} else {
			w.LoopCap = *raw.LoopCap
		}
	}

	w.Roles = []string{}
	if len(raw.Roles) == 0 {
		add("roles", "a workflow needs at least one role")
	}
	if len(raw.Roles) > MaxWorkflowRoles {
		add("roles", "too many roles (%d); at most %d", len(raw.Roles), MaxWorkflowRoles)
	}
	declared := map[string]bool{}
	for i, id := range raw.Roles {
		field := fmt.Sprintf("roles[%d]", i)
		switch {
		case id == ReviewStep:
			add(field, "%q is reserved for the review step; list it only at the end of order", ReviewStep)
		case !idPattern.MatchString(id):
			add(field, "role id %q must be lowercase letters, digits, and hyphens", id)
		case declared[id]:
			add(field, "duplicate role %q", id)
		default:
			declared[id] = true
			w.Roles = append(w.Roles, id)
		}
	}

	w.Order = []string{}
	position := map[string]int{}
	if len(raw.Order) == 0 {
		add("order", "order is required: list each role once, optionally ending with %q", ReviewStep)
	}
	for i, id := range raw.Order {
		field := fmt.Sprintf("order[%d]", i)
		switch {
		case id == ReviewStep:
			if i != len(raw.Order)-1 {
				add(field, "%q may only be the last step of the order", ReviewStep)
			}
		case !declared[id]:
			add(field, "%q is not one of the declared roles", id)
			continue
		}
		if _, dup := position[id]; dup {
			add(field, "%q appears more than once in the forward order; list each role once", id)
			continue
		}
		position[id] = i
		w.Order = append(w.Order, id)
	}
	for _, id := range w.Roles {
		if _, ok := position[id]; !ok && len(raw.Order) > 0 {
			add("order", "role %q is declared but missing from the forward order", id)
		}
	}

	w.Returns = []Return{}
	froms := make([]string, 0, len(raw.Returns))
	for from := range raw.Returns {
		froms = append(froms, from)
	}
	sort.Slice(froms, func(i, j int) bool {
		pi, iok := position[froms[i]]
		pj, jok := position[froms[j]]
		if iok != jok {
			return iok
		}
		if pi != pj {
			return pi < pj
		}
		return froms[i] < froms[j]
	})
	for _, from := range froms {
		field := "returns." + from
		fromPos, ok := position[from]
		if !ok {
			if from == ReviewStep {
				add(field, "returns names %q but the order has no review step", ReviewStep)
			} else {
				add(field, "returns names %q, which is not in the forward order", from)
			}
			continue
		}
		targets := raw.Returns[from]
		if len(targets) == 0 {
			add(field, "list at least one earlier role, or remove the entry")
			continue
		}
		seen := map[string]bool{}
		ret := Return{From: from, To: []string{}}
		for i, to := range targets {
			tf := fmt.Sprintf("%s[%d]", field, i)
			toPos, ok := position[to]
			switch {
			case seen[to]:
				add(tf, "duplicate return target %q", to)
			case to == ReviewStep:
				add(tf, "the baton cannot be returned to the review step")
			case !ok:
				add(tf, "return target %q is not in the forward order", to)
			case toPos >= fromPos:
				add(tf, "%q must be an earlier role than %q; returns only go backward", to, from)
			default:
				ret.To = append(ret.To, to)
			}
			seen[to] = true
		}
		w.Returns = append(w.Returns, ret)
	}
	entry.Workflow = &w
	return entry
}

// crossValidateWorkflows resolves each workflow role against the discovered,
// valid role definitions.
func crossValidateWorkflows(cat Catalog) {
	for i := range cat.Workflows {
		we := &cat.Workflows[i]
		if we.Workflow == nil {
			continue
		}
		for ri, id := range we.Workflow.Roles {
			field := fmt.Sprintf("roles[%d]", ri)
			re, ok := cat.FindRole(id)
			switch {
			case !ok:
				we.Diagnostics = append(we.Diagnostics, Diagnostic{File: we.File, Field: field, Message: fmt.Sprintf("references unknown role %q; add %s/%s.yaml or fix the id", id, RolesDir, id)})
			case len(re.Diagnostics) > 0:
				we.Diagnostics = append(we.Diagnostics, Diagnostic{File: we.File, Field: field, Message: fmt.Sprintf("role %q is invalid; fix %s first", id, re.File)})
			}
		}
	}
}

func flagDuplicateRoles(entries []RoleEntry) {
	byID := map[string][]int{}
	for i, e := range entries {
		byID[e.ID] = append(byID[e.ID], i)
	}
	for id, idxs := range byID {
		if len(idxs) < 2 {
			continue
		}
		for _, i := range idxs {
			entries[i].Diagnostics = append(entries[i].Diagnostics, Diagnostic{File: entries[i].File, Field: "id", Message: fmt.Sprintf("duplicate role id %q is also defined in %s; ids must be unique", id, otherFiles(entries, idxs, i))})
		}
	}
}

func flagDuplicateWorkflows(entries []WorkflowEntry) {
	byID := map[string][]int{}
	for i, e := range entries {
		byID[e.ID] = append(byID[e.ID], i)
	}
	for id, idxs := range byID {
		if len(idxs) < 2 {
			continue
		}
		for _, i := range idxs {
			var others []string
			for _, j := range idxs {
				if j != i {
					others = append(others, entries[j].File)
				}
			}
			entries[i].Diagnostics = append(entries[i].Diagnostics, Diagnostic{File: entries[i].File, Field: "id", Message: fmt.Sprintf("duplicate workflow id %q is also defined in %s; ids must be unique", id, strings.Join(others, ", "))})
		}
	}
}

func otherFiles(entries []RoleEntry, idxs []int, self int) string {
	var others []string
	for _, j := range idxs {
		if j != self {
			others = append(others, entries[j].File)
		}
	}
	return strings.Join(others, ", ")
}

// --- shared helpers -------------------------------------------------------

func checkVersion(v *int, add func(field, format string, args ...any)) {
	switch {
	case v == nil:
		add("version", "version is required (supported: %d)", FormatVersion)
	case *v != FormatVersion:
		add("version", "unsupported version %d (supported: %d)", *v, FormatVersion)
	}
}

func checkID(id, what string, add func(field, format string, args ...any)) {
	if !idPattern.MatchString(id) {
		add("id", "%s id %q must be 1-64 lowercase letters, digits, and hyphens", what, id)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileStem(file string) string {
	base := path.Base(file)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
}

// resolveInstructions returns the resolved instruction text, its provenance
// label, and a content hash. Exactly one of inline or file must be set.
func (d *discoverer) resolveInstructions(inline, file string, add func(field, format string, args ...any)) (text, source, sum string, ok bool) {
	switch {
	case inline != "" && file != "":
		add("instructions", "specify exactly one of instructions or instructionsFile, not both")
		return "", "", "", false
	case inline != "":
		text, source = inline, "inline"
	default:
		if err := validateRepoRelativePath(file); err != nil {
			add("instructionsFile", "%q: %v", file, err)
			return "", "", "", false
		}
		data, err := d.readRegularFile(file, MaxInstructionBytes)
		if err != nil {
			add("instructionsFile", "%v", err)
			return "", "", "", false
		}
		if !utf8.Valid(data) {
			add("instructionsFile", "%s is not valid UTF-8", file)
			return "", "", "", false
		}
		text, source = string(data), file
	}
	if len(text) > MaxInstructionBytes {
		add("instructions", "instructions are too long (limit %d bytes)", MaxInstructionBytes)
		return "", "", "", false
	}
	if strings.TrimSpace(text) == "" {
		add("instructions", "instructions are empty")
		return "", "", "", false
	}
	h := sha256.Sum256([]byte(text))
	return text, source, hex.EncodeToString(h[:]), true
}

// validateRepoRelativePath refuses absolute paths, traversal, and non-canonical
// spellings so a definition cannot reference anything outside the repository.
func validateRepoRelativePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path is empty")
	}
	if p != strings.TrimSpace(p) {
		return errors.New("path must not have leading or trailing whitespace")
	}
	if strings.ContainsAny(p, "\\\x00") {
		return errors.New("path must use forward slashes and contain no NUL bytes")
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return errors.New("path must be repo-relative, not absolute")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "..":
			return errors.New("path must not contain .. traversal")
		case ".", "":
			return errors.New("path must be canonical (no empty or . segments)")
		}
	}
	return nil
}

// validateGlob checks one allowedPaths pattern. Syntax: path.Match segments,
// plus a whole-segment ** that matches any number of directories.
func validateGlob(g string) error {
	if err := validateRepoRelativePath(g); err != nil {
		return err
	}
	if strings.HasPrefix(g, "!") {
		return errors.New("negated patterns are not supported; list the paths that may change")
	}
	segs := strings.Split(g, "/")
	if segs[0] == ".git" {
		return errors.New("patterns must not target .git")
	}
	for _, s := range segs {
		if s == "**" {
			continue
		}
		if strings.Contains(s, "**") {
			return fmt.Errorf("** must be a whole path segment, found %q", s)
		}
		if _, err := path.Match(s, ""); err != nil {
			return fmt.Errorf("invalid pattern segment %q: %w", s, err)
		}
	}
	return nil
}
