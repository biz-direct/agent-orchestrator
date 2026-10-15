import { useTranslation } from "react-i18next";
import { Button } from "../ui/button";

/**
 * Explicit save controls shared by the global and project prompt pages. Nothing
 * saves until Save is clicked (or Cmd/Ctrl+S while focus is inside the form).
 */
export function PromptsSaveBar({
	dirty,
	saving,
	blocked,
	saved,
	error,
	onSave,
	onDiscard,
}: {
	dirty: boolean;
	saving: boolean;
	/** The draft cannot be saved as it stands (e.g. over the size limit). */
	blocked?: boolean;
	saved: boolean;
	error?: string | null;
	onSave: () => void;
	onDiscard: () => void;
}) {
	const { t } = useTranslation();
	const status = saving
		? t("settings.prompts.saving")
		: dirty
			? t("settings.prompts.unsaved")
			: saved
				? t("settings.prompts.saved")
				: "";
	return (
		<div className="flex flex-col gap-2">
			{error ? <p role="alert" className="text-xs text-destructive">{error}</p> : null}
			<div className="flex items-center justify-end gap-3">
				<p role="status" className="mr-auto text-xs text-muted-foreground">{status}</p>
				<Button type="button" size="sm" variant="secondary" disabled={!dirty || saving} onClick={onDiscard}>
					{t("settings.prompts.discard")}
				</Button>
				<Button type="button" size="sm" disabled={!dirty || saving || blocked} onClick={onSave}>
					{t("settings.prompts.save")}
				</Button>
			</div>
		</div>
	);
}

/** True for the Cmd/Ctrl+S chord. */
export function isSaveShortcut(event: { key: string; metaKey: boolean; ctrlKey: boolean; altKey: boolean }): boolean {
	return (event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === "s";
}
