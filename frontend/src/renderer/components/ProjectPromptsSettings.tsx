import { useState } from "react";
import { ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useSettings } from "../hooks/useSettings";
import { cn } from "../lib/utils";
import { MAX_ORCHESTRATOR_RULES_BYTES, orchestratorRulesBytes } from "./settings/PromptsSettingsSection";
import { Switch } from "./ui/switch";
import { Textarea } from "./ui/textarea";

/**
 * Project orchestrator prompt: the host's global prompt (read-only, collapsed),
 * the per-project opt-out, and the project's own `orchestratorRules`. Edits flow
 * through the project form's autosave; they never replace a running orchestrator.
 */
export function ProjectPromptsSettings({
	hostId,
	orchestratorRules,
	skipGlobal,
	onOrchestratorRulesChange,
	onSkipGlobalChange,
}: {
	hostId?: string;
	orchestratorRules: string;
	skipGlobal: boolean;
	onOrchestratorRulesChange: (value: string) => void;
	onSkipGlobalChange: (value: boolean) => void;
}) {
	const { t } = useTranslation();
	const { settings } = useSettings(hostId);
	const [previewOpen, setPreviewOpen] = useState(false);
	const globalRules = (settings?.globalOrchestratorRules ?? "").trim();
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

			<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.project.takesEffect")}</p>
		</div>
	);
}
