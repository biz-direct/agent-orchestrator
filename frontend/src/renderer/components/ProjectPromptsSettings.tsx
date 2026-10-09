import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { fetchOrchestratorRulesFile, orchestratorRulesFileQueryKey } from "../hooks/useOrchestratorRulesFile";
import { useSettings } from "../hooks/useSettings";
import { cn } from "../lib/utils";
import { MAX_ORCHESTRATOR_RULES_BYTES, orchestratorRulesBytes } from "./settings/PromptsSettingsSection";
import { Input } from "./ui/input";
import { Switch } from "./ui/switch";
import { Textarea } from "./ui/textarea";

/**
 * Project orchestrator prompt: the host's global prompt (read-only, collapsed),
 * the per-project opt-out, and the project's own `orchestratorRules`. Edits flow
 * through the project form's autosave; they never replace a running orchestrator.
 */
export function ProjectPromptsSettings({
	projectId,
	hostId,
	orchestratorRules,
	orchestratorRulesFile,
	rulesFileError,
	skipGlobal,
	onOrchestratorRulesChange,
	onOrchestratorRulesFileChange,
	onSkipGlobalChange,
}: {
	projectId: string;
	hostId?: string;
	orchestratorRules: string;
	orchestratorRulesFile: string;
	/** Set when saving was blocked because the file path did not validate. */
	rulesFileError?: string | null;
	skipGlobal: boolean;
	onOrchestratorRulesChange: (value: string) => void;
	onOrchestratorRulesFileChange: (value: string) => void;
	onSkipGlobalChange: (value: boolean) => void;
}) {
	const { t } = useTranslation();
	const { settings, isLoading, error } = useSettings(hostId);
	const [previewOpen, setPreviewOpen] = useState(false);
	const globalRules = (settings?.globalOrchestratorRules ?? "").trim();
	const trimmedFile = orchestratorRulesFile.trim();
	const [previewPath, setPreviewPath] = useState(trimmedFile);
	useEffect(() => {
		const timeout = window.setTimeout(() => setPreviewPath(trimmedFile), 400);
		return () => window.clearTimeout(timeout);
	}, [trimmedFile]);
	const filePreview = useQuery({
		queryKey: orchestratorRulesFileQueryKey(projectId, hostId, previewPath),
		queryFn: () => fetchOrchestratorRulesFile(projectId, hostId, previewPath),
		enabled: previewPath !== "",
		retry: false,
	});
	const fileMessage = rulesFileError ?? (previewPath === trimmedFile && filePreview.error instanceof Error ? filePreview.error.message : null);
	const tooLarge = orchestratorRulesBytes(orchestratorRules) > MAX_ORCHESTRATOR_RULES_BYTES;

	return (
		<div className="flex w-full flex-col gap-4 rounded-md bg-[var(--color-bg-settings-row)] p-3">
			<section className="flex flex-col gap-2" aria-label={t("settings.prompts.project.globalPreview")}>
				<button
					type="button"
					className="flex items-center gap-1.5 text-left text-sm font-medium text-foreground"
					aria-expanded={previewOpen}
					onClick={() => setPreviewOpen((open) => !open)}
				>
					<ChevronRight className={cn("size-3.5 transition-transform", previewOpen && "rotate-90")} aria-hidden="true" />
					<span>{t("settings.prompts.project.globalPreview")}</span>
					{skipGlobal ? <span className="text-xs font-normal text-muted-foreground">{t("settings.prompts.project.skipped")}</span> : null}
				</button>
				{previewOpen ? (
					globalRules ? (
						<pre
							data-testid="global-prompt-preview"
							className={cn(
								"max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-input/50 px-3 py-2 font-mono text-xs text-foreground",
								skipGlobal && "opacity-50",
							)}
						>
							{globalRules}
						</pre>
					) : isLoading ? null : error || !settings ? (
						<p role="alert" className="text-xs text-destructive">{t("settings.prompts.project.globalLoadFailed")}</p>
					) : (
						<p className="text-xs text-muted-foreground">{t("settings.prompts.project.globalEmpty")}</p>
					)
				) : null}
			</section>

			<div className="flex items-center justify-between gap-3">
				<label htmlFor="skip-global-orchestrator-prompt" className="text-sm text-foreground">
					{t("settings.prompts.project.skipGlobal")}
				</label>
				<Switch
					id="skip-global-orchestrator-prompt"
					checked={skipGlobal}
					onCheckedChange={onSkipGlobalChange}
				/>
			</div>

			<div className="flex flex-col gap-2">
				<label htmlFor="project-orchestrator-prompt" className="text-sm font-medium text-foreground">
					{t("settings.prompts.project.label")}
				</label>
				<Textarea
					id="project-orchestrator-prompt"
					className="min-h-48 font-mono text-xs"
					value={orchestratorRules}
					aria-invalid={tooLarge}
					onChange={(event) => onOrchestratorRulesChange(event.target.value)}
				/>
				{tooLarge ? <p role="alert" className="text-xs text-destructive">{t("settings.prompts.tooLarge")}</p> : null}
			</div>

			<div className="flex flex-col gap-2">
				<label htmlFor="project-orchestrator-rules-file" className="text-sm font-medium text-foreground">
					{t("settings.prompts.project.rulesFile")}
				</label>
				<Input
					id="project-orchestrator-rules-file"
					className="font-mono text-xs"
					value={orchestratorRulesFile}
					placeholder="docs/orchestrator-rules.md"
					aria-invalid={Boolean(fileMessage)}
					onChange={(event) => onOrchestratorRulesFileChange(event.target.value)}
				/>
				{fileMessage ? <p role="alert" className="text-xs text-destructive">{fileMessage}</p> : null}
				{trimmedFile && !fileMessage && previewPath === trimmedFile && filePreview.data !== undefined ? (
					<pre
						data-testid="rules-file-preview"
						className="max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-input/50 px-3 py-2 font-mono text-xs text-foreground"
					>
						{filePreview.data.trim() === "" ? t("settings.prompts.project.rulesFileEmpty") : filePreview.data}
					</pre>
				) : null}
			</div>

			<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.project.takesEffect")}</p>
		</div>
	);
}
