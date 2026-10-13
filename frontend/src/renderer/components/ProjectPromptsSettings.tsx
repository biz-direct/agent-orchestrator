import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { fetchOrchestratorRulesFile, orchestratorRulesFileQueryKey } from "../hooks/useOrchestratorRulesFile";
import { agentRulesFileQueryKey, fetchAgentRulesFile, invalidatePromptViews, type PromptRole } from "../hooks/usePromptViews";
import { useSettings } from "../hooks/useSettings";
import { cn } from "../lib/utils";
import type { CommitProjectPrompts, ProjectPromptValues } from "./ProjectSettingsEditor";
import { PromptApplyNotice } from "./settings/PromptApplyNotice";
import { isSaveShortcut, PromptsSaveBar } from "./settings/PromptsSaveBar";
import { MAX_ORCHESTRATOR_RULES_BYTES, orchestratorRulesBytes } from "./settings/PromptsSettingsSection";
import { EffectivePromptPanel } from "./settings/PromptViews";
import { Input } from "./ui/input";
import { Switch } from "./ui/switch";
import { Textarea } from "./ui/textarea";

/**
 * One role's fields: the host's global prompt (read-only, collapsed), the
 * per-project opt-out, the project's own rules, and an optional rules file with
 * a live preview and path validation.
 */
function RolePromptFields({
	role,
	projectId,
	hostId,
	globalRules,
	globalState,
	rules,
	rulesFile,
	skipGlobal,
	onEdit,
	rulesFileError,
	refreshKey,
}: {
	role: PromptRole;
	projectId: string;
	hostId?: string;
	globalRules: string;
	globalState: "loading" | "failed" | "ready";
	rules: string;
	rulesFile: string;
	skipGlobal: boolean;
	onEdit: (patch: { rules?: string; rulesFile?: string; skipGlobal?: boolean }) => void;
	rulesFileError?: string | null;
	refreshKey: number;
}) {
	const { t } = useTranslation();
	const worker = role === "worker";
	const prefix = worker ? "project-worker" : "project-orchestrator";
	const [previewOpen, setPreviewOpen] = useState(false);
	const trimmedFile = rulesFile.trim();
	const [previewPath, setPreviewPath] = useState(trimmedFile);
	useEffect(() => {
		const timeout = window.setTimeout(() => setPreviewPath(trimmedFile), 400);
		return () => window.clearTimeout(timeout);
	}, [trimmedFile]);
	const filePreview = useQuery({
		queryKey: worker ? agentRulesFileQueryKey(projectId, hostId, previewPath) : orchestratorRulesFileQueryKey(projectId, hostId, previewPath),
		queryFn: () => (worker ? fetchAgentRulesFile : fetchOrchestratorRulesFile)(projectId, hostId, previewPath),
		enabled: previewPath !== "",
		retry: false,
	});
	const fileMessage = rulesFileError ?? (previewPath === trimmedFile && filePreview.error instanceof Error ? filePreview.error.message : null);
	const tooLarge = orchestratorRulesBytes(rules) > MAX_ORCHESTRATOR_RULES_BYTES;
	const trimmedGlobal = globalRules.trim();
	return (
		<div className="flex flex-col gap-4" role="group" aria-label={t(worker ? "settings.prompts.project.workerHeading" : "settings.prompts.project.orchestratorHeading")}>
			<h3 className="text-sm font-semibold text-foreground">{t(worker ? "settings.prompts.project.workerHeading" : "settings.prompts.project.orchestratorHeading")}</h3>
			<section className="flex flex-col gap-2" aria-label={t(worker ? "settings.prompts.project.workerGlobalPreview" : "settings.prompts.project.globalPreview")}>
				<button
					type="button"
					className="flex items-center gap-1.5 text-left text-sm font-medium text-foreground"
					aria-expanded={previewOpen}
					onClick={() => setPreviewOpen((open) => !open)}
				>
					<ChevronRight className={cn("size-3.5 transition-transform", previewOpen && "rotate-90")} aria-hidden="true" />
					<span>{t(worker ? "settings.prompts.project.workerGlobalPreview" : "settings.prompts.project.globalPreview")}</span>
					{skipGlobal ? <span className="text-xs font-normal text-muted-foreground">{t("settings.prompts.project.skipped")}</span> : null}
				</button>
				{previewOpen ? (
					trimmedGlobal ? (
						<pre
							data-testid={worker ? "global-worker-prompt-preview" : "global-prompt-preview"}
							className={cn(
								"max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-input/50 px-3 py-2 font-mono text-xs text-foreground",
								skipGlobal && "opacity-50",
							)}
						>
							{trimmedGlobal}
						</pre>
					) : globalState === "loading" ? null : globalState === "failed" ? (
						<p role="alert" className="text-xs text-destructive">{t(worker ? "settings.prompts.project.workerGlobalLoadFailed" : "settings.prompts.project.globalLoadFailed")}</p>
					) : (
						<p className="text-xs text-muted-foreground">{t(worker ? "settings.prompts.project.workerGlobalEmpty" : "settings.prompts.project.globalEmpty")}</p>
					)
				) : null}
			</section>

			<div className="flex items-center justify-between gap-3">
				<label htmlFor={`skip-global-${role}-prompt`} className="text-sm text-foreground">
					{t(worker ? "settings.prompts.project.skipGlobalWorker" : "settings.prompts.project.skipGlobal")}
				</label>
				<Switch id={`skip-global-${role}-prompt`} checked={skipGlobal} onCheckedChange={(value) => onEdit({ skipGlobal: value })} />
			</div>

			<div className="flex flex-col gap-2">
				<label htmlFor={`${prefix}-prompt`} className="text-sm font-medium text-foreground">
					{t(worker ? "settings.prompts.project.workerLabel" : "settings.prompts.project.label")}
				</label>
				<Textarea
					id={`${prefix}-prompt`}
					className="min-h-48 font-mono text-xs"
					value={rules}
					aria-invalid={tooLarge}
					onChange={(event) => onEdit({ rules: event.target.value })}
				/>
				{tooLarge ? <p role="alert" className="text-xs text-destructive">{t("settings.prompts.tooLarge")}</p> : null}
			</div>

			<div className="flex flex-col gap-2">
				<label htmlFor={`${prefix}-rules-file`} className="text-sm font-medium text-foreground">
					{t(worker ? "settings.prompts.project.workerRulesFile" : "settings.prompts.project.rulesFile")}
				</label>
				<Input
					id={`${prefix}-rules-file`}
					className="font-mono text-xs"
					value={rulesFile}
					placeholder={worker ? "docs/worker-rules.md" : "docs/orchestrator-rules.md"}
					aria-invalid={Boolean(fileMessage)}
					aria-describedby={fileMessage ? `${prefix}-rules-file-error` : undefined}
					onChange={(event) => onEdit({ rulesFile: event.target.value })}
				/>
				{fileMessage ? <p id={`${prefix}-rules-file-error`} role="alert" className="text-xs text-destructive">{fileMessage}</p> : null}
				{trimmedFile && !fileMessage && previewPath === trimmedFile && filePreview.data !== undefined ? (
					<pre
						data-testid={worker ? "worker-rules-file-preview" : "rules-file-preview"}
						className="max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-input/50 px-3 py-2 font-mono text-xs text-foreground"
					>
						{filePreview.data.trim() === "" ? t("settings.prompts.project.rulesFileEmpty") : filePreview.data}
					</pre>
				) : null}
			</div>

			<p className="text-xs leading-relaxed text-muted-foreground">{t(worker ? "settings.prompts.project.workerTakesEffect" : "settings.prompts.project.takesEffect")}</p>

			<EffectivePromptPanel projectId={projectId} role={role} hostId={hostId} refreshKey={refreshKey} />
		</div>
	);
}

/**
 * Project prompts: an Orchestrator section and a Worker section. Edits stay
 * local until Save is clicked (or Cmd/Ctrl+S); they never replace a running
 * session, and a refetch of the saved values never overwrites them.
 */
export function ProjectPromptsSettings({
	projectId,
	hostId,
	saved,
	edits,
	onEditsChange,
	onSave,
	rulesFileError,
}: {
	projectId: string;
	hostId?: string;
	saved: ProjectPromptValues;
	/** Unsaved values; null while nothing has been edited. */
	edits: ProjectPromptValues | null;
	onEditsChange: (next: ProjectPromptValues | null) => void;
	onSave: CommitProjectPrompts;
	/** Set when saving was blocked because a file path did not validate. */
	rulesFileError?: string | null;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [saving, setSaving] = useState(false);
	const [justSaved, setJustSaved] = useState(false);
	const [saveError, setSaveError] = useState<string | null>(null);
	const [savedCount, setSavedCount] = useState(0);
	const current = edits ?? saved;
	const latestEditsRef = useRef(edits);
	latestEditsRef.current = edits;
	const orchestratorRules = current.orchestratorRules ?? "";
	const orchestratorRulesFile = current.orchestratorRulesFile ?? "";
	const skipGlobalOrchestrator = current.skipGlobalOrchestratorRules ?? false;
	const agentRules = current.agentRules ?? "";
	const agentRulesFile = current.agentRulesFile ?? "";
	const skipGlobalWorker = current.skipGlobalWorkerRules ?? false;
	const differs = (a: string | undefined, b: string | undefined) => (a ?? "").trim() !== (b ?? "").trim();
	const dirty =
		edits !== null &&
		(differs(orchestratorRules, saved.orchestratorRules) ||
			differs(orchestratorRulesFile, saved.orchestratorRulesFile) ||
			skipGlobalOrchestrator !== (saved.skipGlobalOrchestratorRules ?? false) ||
			differs(agentRules, saved.agentRules) ||
			differs(agentRulesFile, saved.agentRulesFile) ||
			skipGlobalWorker !== (saved.skipGlobalWorkerRules ?? false));
	const edit = (patch: Partial<ProjectPromptValues>) => {
		setJustSaved(false);
		setSaveError(null);
		onEditsChange({ ...current, ...patch });
	};
	const { settings, isLoading, error } = useSettings(hostId);
	const globalState = isLoading ? "loading" : error || !settings ? "failed" : "ready";
	const tooLarge =
		orchestratorRulesBytes(orchestratorRules) > MAX_ORCHESTRATOR_RULES_BYTES || orchestratorRulesBytes(agentRules) > MAX_ORCHESTRATOR_RULES_BYTES;
	const save = () => {
		if (!dirty || saving || tooLarge || edits === null) return;
		setSaving(true);
		setSaveError(null);
		const submitted = edits;
		onSave(submitted).then(
			() => {
				void invalidatePromptViews(queryClient);
				setSavedCount((count) => count + 1);
				// Acknowledge only the submitted snapshot; text typed since stays unsaved.
				if (latestEditsRef.current === submitted) {
					onEditsChange(null);
					setJustSaved(true);
				}
			},
			(cause: unknown) => setSaveError(cause instanceof Error ? cause.message : t("settings.project.saveFailed")),
		).finally(() => setSaving(false));
	};
	const discard = () => {
		setSaveError(null);
		onEditsChange(null);
	};
	// The daemon's message names the failing field (agentRulesFile / orchestratorRulesFile).
	const workerFileError = rulesFileError?.includes("agentRulesFile") ? rulesFileError : null;
	const orchestratorFileError = rulesFileError && !workerFileError ? rulesFileError : null;

	return (
		<div
			className="flex w-full flex-col gap-6 rounded-md bg-[var(--color-bg-settings-row)] p-3"
			onKeyDown={(event) => {
				if (!isSaveShortcut(event)) return;
				event.preventDefault();
				save();
			}}
		>
			<RolePromptFields
				role="orchestrator"
				projectId={projectId}
				hostId={hostId}
				globalRules={settings?.globalOrchestratorRules ?? ""}
				globalState={globalState}
				rules={orchestratorRules}
				rulesFile={orchestratorRulesFile}
				skipGlobal={skipGlobalOrchestrator}
				onEdit={(patch) => edit({
					...(patch.rules !== undefined && { orchestratorRules: patch.rules }),
					...(patch.rulesFile !== undefined && { orchestratorRulesFile: patch.rulesFile }),
					...(patch.skipGlobal !== undefined && { skipGlobalOrchestratorRules: patch.skipGlobal }),
				})}
				rulesFileError={orchestratorFileError}
				refreshKey={savedCount}
			/>
			<RolePromptFields
				role="worker"
				projectId={projectId}
				hostId={hostId}
				globalRules={settings?.globalWorkerRules ?? ""}
				globalState={globalState}
				rules={agentRules}
				rulesFile={agentRulesFile}
				skipGlobal={skipGlobalWorker}
				onEdit={(patch) => edit({
					...(patch.rules !== undefined && { agentRules: patch.rules }),
					...(patch.rulesFile !== undefined && { agentRulesFile: patch.rulesFile }),
					...(patch.skipGlobal !== undefined && { skipGlobalWorkerRules: patch.skipGlobal }),
				})}
				rulesFileError={workerFileError}
				refreshKey={savedCount}
			/>

			<PromptsSaveBar
				dirty={dirty}
				saving={saving}
				blocked={tooLarge}
				saved={justSaved}
				error={saveError !== rulesFileError ? saveError : null}
				onSave={save}
				onDiscard={discard}
			/>
			{justSaved ? <PromptApplyNotice roles={["orchestrator", "worker"]} projectId={projectId} hostId={hostId} /> : null}
		</div>
	);
}
