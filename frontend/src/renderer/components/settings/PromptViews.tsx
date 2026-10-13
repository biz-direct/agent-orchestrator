import { ChevronRight, Copy } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useBuiltinPrompts, useEffectivePrompt, type PromptRole, type PromptSection } from "../../hooks/usePromptViews";
import { aoBridge } from "../../lib/bridge";
import { cn } from "../../lib/utils";
import { Button } from "../ui/button";

/** Copies text through the desktop bridge and flips to "Copied" briefly. */
function CopyButton({ text, label, copyLabel }: { text: string; label: string; copyLabel?: string }) {
	const { t } = useTranslation();
	const [copied, setCopied] = useState(false);
	const timeout = useRef<number | null>(null);
	useEffect(() => () => {
		if (timeout.current !== null) window.clearTimeout(timeout.current);
	}, []);
	const copy = async () => {
		await aoBridge.clipboard.writeText(text);
		setCopied(true);
		if (timeout.current !== null) window.clearTimeout(timeout.current);
		timeout.current = window.setTimeout(() => setCopied(false), 2_000);
	};
	return (
		<Button type="button" size="sm" variant="secondary" aria-label={copyLabel ?? label} onClick={() => void copy()}>
			<Copy className="size-3.5" aria-hidden="true" />
			{copied ? t("settings.prompts.copied") : label}
		</Button>
	);
}

/** A titled, collapsed-by-default block. */
export function PromptDisclosure({
	title,
	hint,
	defaultOpen = false,
	onOpenChange,
	actions,
	children,
}: {
	title: string;
	hint?: ReactNode;
	defaultOpen?: boolean;
	onOpenChange?: (open: boolean) => void;
	actions?: ReactNode;
	children: ReactNode;
}) {
	const [open, setOpen] = useState(defaultOpen);
	return (
		<section className="flex flex-col gap-2" aria-label={title}>
			<div className="flex items-center justify-between gap-3">
				<button
					type="button"
					className="flex min-w-0 items-center gap-1.5 text-left text-sm font-medium text-foreground"
					aria-expanded={open}
					onClick={() => {
						setOpen(!open);
						onOpenChange?.(!open);
					}}
				>
					<ChevronRight className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} aria-hidden="true" />
					<span className="truncate">{title}</span>
					{hint}
				</button>
				{open ? actions : null}
			</div>
			{open ? children : null}
		</section>
	);
}

/** The prompt's blocks, each collapsible, labelled with the layer it came from, and copyable. */
export function PromptSectionsView({ sections, testId }: { sections: PromptSection[]; testId?: string }) {
	const { t } = useTranslation();
	return (
		<div className="flex flex-col gap-2 pl-5" data-testid={testId}>
			{sections.map((section, index) => {
				const title = section.title || t("settings.prompts.untitled");
				return (
					<PromptDisclosure
						key={`${index}-${section.layer}-${title}`}
						title={title}
						hint={<span className="text-xs font-normal text-muted-foreground" data-testid="prompt-layer">{t(`settings.prompts.layer.${section.layer}`)}</span>}
						actions={<CopyButton text={section.text} label={t("settings.prompts.copy")} copyLabel={t("settings.prompts.copySection", { title })} />}
					>
						<pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-md bg-input/50 px-3 py-2 font-mono text-xs text-foreground">{section.text}</pre>
					</PromptDisclosure>
				);
			})}
		</div>
	);
}

/** Read-only built-in orchestrator and worker prompts, rendered by the daemon with sample project values. */
export function BuiltinPromptsPanel() {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const query = useBuiltinPrompts(undefined, open);
	const data = query.data;
	const groups: Array<{ key: string; title: string; sections: PromptSection[] }> = data
		? [
				{ key: "orchestrator", title: t("settings.prompts.builtin.orchestrator"), sections: data.orchestrator },
				{ key: "worker", title: t("settings.prompts.builtin.worker"), sections: data.worker },
				{ key: "standalone", title: t("settings.prompts.builtin.standalone"), sections: data.standaloneWorker },
			]
		: [];
	return (
		<div className="flex w-full flex-col gap-2 rounded-md bg-[var(--color-bg-settings-row)] p-3">
			<PromptDisclosure title={t("settings.prompts.builtin.title")} onOpenChange={setOpen}>
				<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.builtin.help")}</p>
				{data ? (
					<p className="text-xs leading-relaxed text-muted-foreground" data-testid="builtin-sample">
						{t("settings.prompts.builtin.sample", {
							name: data.sample.projectName,
							repo: data.sample.repo,
							branch: data.sample.defaultBranch,
						})}
					</p>
				) : null}
				{query.isError ? <p role="alert" className="text-xs text-destructive">{t("settings.prompts.builtin.loadFailed")}</p> : null}
				{query.isLoading ? <p className="text-xs text-muted-foreground">{t("settings.prompts.builtin.loading")}</p> : null}
				{groups.map((group) => (
					<div key={group.key} className="flex flex-col gap-2 pl-5">
						<PromptDisclosure
							title={group.title}
							actions={<CopyButton text={group.sections.map((section) => section.text).join("\n\n")} label={t("settings.prompts.copyAll")} copyLabel={t("settings.prompts.copyAllNamed", { title: group.title })} />}
						>
							<PromptSectionsView sections={group.sections} testId={`builtin-${group.key}`} />
						</PromptDisclosure>
					</div>
				))}
			</PromptDisclosure>
		</div>
	);
}

/** The fully assembled prompt a new session of `role` would get in this project, layer by layer. */
export function EffectivePromptPanel({ projectId, role, hostId, refreshKey }: { projectId: string; role: PromptRole; hostId?: string; refreshKey?: number }) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const query = useEffectivePrompt(projectId, role, hostId, open);
	const { refetch } = query;
	// Re-read after a save so the view never shows rules from before it.
	useEffect(() => {
		if (open && refreshKey) void refetch();
	}, [open, refreshKey, refetch]);
	const title = t(role === "orchestrator" ? "settings.prompts.effective.orchestrator" : "settings.prompts.effective.worker");
	return (
		<PromptDisclosure
			title={title}
			onOpenChange={setOpen}
			actions={query.data ? <CopyButton text={query.data.text} label={t("settings.prompts.copyAll")} copyLabel={t("settings.prompts.copyAllNamed", { title })} /> : null}
		>
			<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.effective.help")}</p>
			{query.isError ? (
				<p role="alert" className="text-xs text-destructive">
					{t("settings.prompts.effective.loadFailed", { message: query.error instanceof Error ? query.error.message : "" })}
				</p>
			) : null}
			{query.isLoading ? <p className="text-xs text-muted-foreground">{t("settings.prompts.builtin.loading")}</p> : null}
			{query.data ? <PromptSectionsView sections={query.data.sections} testId={`effective-${role}`} /> : null}
		</PromptDisclosure>
	);
}
