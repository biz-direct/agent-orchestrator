import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSettings, useUpdateGlobalOrchestratorRules } from "../../hooks/useSettings";
import { apiErrorMessage } from "../../lib/api-client";
import { Textarea } from "../ui/textarea";
import { isSaveShortcut, PromptsSaveBar } from "./PromptsSaveBar";
import { SettingsSection } from "./SettingsSection";

/** Mirrors the daemon's 32 KiB cap on one orchestrator rules value. */
export const MAX_ORCHESTRATOR_RULES_BYTES = 32 * 1024;

export function orchestratorRulesBytes(value: string): number {
	return new TextEncoder().encode(value.trim()).length;
}

/**
 * Global orchestrator prompt. It is this computer's daemon value only: remote
 * hosts keep their own, and running orchestrators pick the change up the next
 * time one is spawned or restored.
 */
export function PromptsSettingsSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const { settings, isLoading, error } = useSettings();
	const { update, saving } = useUpdateGlobalOrchestratorRules();
	const saved = settings?.globalOrchestratorRules ?? "";
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
		<SettingsSection titleHidden={titleHidden} title={t("settings.prompts")}>
			<div
				className="flex w-full flex-col gap-2 rounded-md bg-[var(--color-bg-settings-row)] p-3"
				onKeyDown={(event) => {
					if (!isSaveShortcut(event)) return;
					event.preventDefault();
					save();
				}}
			>
				<label htmlFor="global-orchestrator-prompt" className="text-sm font-medium text-foreground">
					{t("settings.prompts.global.label")}
				</label>
				<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.global.help")}</p>
				<Textarea
					id="global-orchestrator-prompt"
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
				<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.global.scope")}</p>
				<PromptsSaveBar
					dirty={dirty}
					saving={saving}
					blocked={tooLarge || isLoading}
					saved={justSaved}
					error={message}
					onSave={save}
					onDiscard={discard}
				/>
			</div>
		</SettingsSection>
	);
}
