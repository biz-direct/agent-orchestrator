import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSettings, useUpdateGlobalOrchestratorRules } from "../../hooks/useSettings";
import { Button } from "../ui/button";
import { Textarea } from "../ui/textarea";
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
	const { update, saving, error: saveError } = useUpdateGlobalOrchestratorRules();
	const saved = settings?.globalOrchestratorRules ?? "";
	const [draft, setDraft] = useState(saved);
	useEffect(() => setDraft(saved), [saved]);

	const tooLarge = orchestratorRulesBytes(draft) > MAX_ORCHESTRATOR_RULES_BYTES;
	const dirty = draft.trim() !== saved.trim();
	const message = tooLarge ? t("settings.prompts.tooLarge") : (saveError ?? error);

	return (
		<SettingsSection titleHidden={titleHidden} title={t("settings.prompts")}>
			<div className="flex w-full flex-col gap-2 rounded-md bg-[var(--color-bg-settings-row)] p-3">
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
					onChange={(event) => setDraft(event.target.value)}
				/>
				<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.global.scope")}</p>
				{message ? (
					<p role="alert" className="text-xs text-destructive">{message}</p>
				) : null}
				<div className="flex justify-end">
					<Button
						type="button"
						size="sm"
						disabled={!dirty || saving || tooLarge || isLoading}
						onClick={() => void update(draft).catch(() => undefined)}
					>
						{t("settings.prompts.save")}
					</Button>
				</div>
			</div>
		</SettingsSection>
	);
}
