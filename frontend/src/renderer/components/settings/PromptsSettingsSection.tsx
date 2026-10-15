import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { invalidatePromptViews, type PromptRole } from "../../hooks/usePromptViews";
import { useSettings, useUpdateGlobalOrchestratorRules, useUpdateGlobalWorkerRules } from "../../hooks/useSettings";
import { apiErrorMessage } from "../../lib/api-client";
import { Textarea } from "../ui/textarea";
import { PromptApplyNotice } from "./PromptApplyNotice";
import { isSaveShortcut, PromptsSaveBar } from "./PromptsSaveBar";
import { BuiltinPromptsPanel } from "./PromptViews";
import { SettingsSection } from "./SettingsSection";

/** Mirrors the daemon's 32 KiB cap on one orchestrator or worker rules value. */
export const MAX_ORCHESTRATOR_RULES_BYTES = 32 * 1024;
export const MAX_WORKER_RULES_BYTES = MAX_ORCHESTRATOR_RULES_BYTES;

export function orchestratorRulesBytes(value: string): number {
	return new TextEncoder().encode(value.trim()).length;
}

/**
 * One global rules editor with the explicit Save/Discard flow. The value is this
 * computer's daemon value only: remote hosts keep their own, and running
 * sessions pick the change up the next time one is spawned or restored.
 */
function GlobalRulesEditor({
	role,
	id,
	label,
	help,
	scope,
	saved,
	isLoading,
	error,
	update,
	saving,
}: {
	role: PromptRole;
	id: string;
	label: string;
	help: string;
	scope: string;
	saved: string;
	isLoading: boolean;
	error?: string;
	update: (rules: string) => Promise<unknown>;
	saving: boolean;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	// `edits` holds the user's unsaved text; null means "show the saved value".
	// A background refetch only changes `saved`, so it can never overwrite edits.
	const [edits, setEdits] = useState<string | null>(null);
	const [justSaved, setJustSaved] = useState(false);
	const [saveError, setSaveError] = useState<string | null>(null);
	const draft = edits ?? saved;
	const latestEditsRef = useRef(edits);
	latestEditsRef.current = edits;

	const tooLarge = orchestratorRulesBytes(draft) > MAX_ORCHESTRATOR_RULES_BYTES;
	const dirty = edits !== null && edits.trim() !== saved.trim();
	const message = tooLarge ? t("settings.prompts.tooLarge") : (saveError ?? error);

	const save = () => {
		if (!dirty || saving || tooLarge || isLoading || edits === null) return;
		setSaveError(null);
		const submitted = edits;
		update(submitted).then(
			() => {
				void invalidatePromptViews(queryClient);
				// Acknowledge only the submitted snapshot; text typed since stays unsaved.
				if (latestEditsRef.current === submitted) {
					setEdits(null);
					setJustSaved(true);
				}
			},
			(cause: unknown) => setSaveError(apiErrorMessage(cause)),
		);
	};
	const discard = () => {
		setEdits(null);
		setSaveError(null);
	};

	return (
		<div
			data-testid={`global-rules-${role}`}
			className="flex w-full flex-col gap-2 rounded-md bg-[var(--color-bg-settings-row)] p-3"
			onKeyDown={(event) => {
				if (!isSaveShortcut(event)) return;
				event.preventDefault();
				save();
			}}
		>
			<label htmlFor={id} className="text-sm font-medium text-foreground">
				{label}
			</label>
			<p className="text-xs leading-relaxed text-muted-foreground">{help}</p>
			<Textarea
				id={id}
				className="min-h-48 font-mono text-xs"
				value={draft}
				disabled={isLoading}
				aria-invalid={tooLarge}
				onChange={(event) => {
					setEdits(event.target.value);
					setJustSaved(false);
					setSaveError(null);
				}}
			/>
			<p className="text-xs leading-relaxed text-muted-foreground">{scope}</p>
			<PromptsSaveBar
				dirty={dirty}
				saving={saving}
				blocked={tooLarge || isLoading}
				saved={justSaved}
				error={message}
				onSave={save}
				onDiscard={discard}
			/>
			{justSaved ? <PromptApplyNotice roles={[role]} /> : null}
		</div>
	);
}

/**
 * Global prompts: read-only built-in prompts (AO's fixed text, shown so you can
 * build on it), plus this computer's global orchestrator and worker rules.
 */
export function PromptsSettingsSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const { settings, isLoading, error } = useSettings();
	const orchestrator = useUpdateGlobalOrchestratorRules();
	const worker = useUpdateGlobalWorkerRules();

	return (
		<SettingsSection titleHidden={titleHidden} title={t("settings.prompts")}>
			<BuiltinPromptsPanel />
			<GlobalRulesEditor
				role="orchestrator"
				id="global-orchestrator-prompt"
				label={t("settings.prompts.global.label")}
				help={t("settings.prompts.global.help")}
				scope={t("settings.prompts.global.scope")}
				saved={settings?.globalOrchestratorRules ?? ""}
				isLoading={isLoading}
				error={error}
				update={orchestrator.update}
				saving={orchestrator.saving}
			/>
			<GlobalRulesEditor
				role="worker"
				id="global-worker-prompt"
				label={t("settings.prompts.globalWorker.label")}
				help={t("settings.prompts.globalWorker.help")}
				scope={t("settings.prompts.global.scope")}
				saved={settings?.globalWorkerRules ?? ""}
				isLoading={isLoading}
				error={error}
				update={worker.update}
				saving={worker.saving}
			/>
		</SettingsSection>
	);
}
