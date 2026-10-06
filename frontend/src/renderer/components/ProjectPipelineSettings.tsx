import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ProjectSettingsSection } from "@aoagents/product-ui";
import { ChevronDown } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { clientForHost } from "../lib/host-clients";
import { cn } from "../lib/utils";
import { SettingsOptionMenu, type SettingsOption } from "./settings/SettingsOptionMenu";
import { Switch } from "./ui/switch";

type Catalog = components["schemas"]["PipelinesCatalogResponse"];
type DefaultStatus = components["schemas"]["PipelinesDefaultStatus"];
type Diagnostic = components["schemas"]["PipelineDiagnostic"];

const UNSET = "unset";
const NORMAL = "normal";
const WORKFLOW_PREFIX = "workflow:";

export const projectPipelinesQueryKey = (projectId: string, hostId?: string) =>
	hostId ? (["project-pipelines", hostId, projectId] as const) : (["project-pipelines", projectId] as const);

export function selectionValue(status: DefaultStatus | undefined): string {
	const selection = status?.selection;
	if (!selection) return UNSET;
	return selection.mode === "workflow" ? `${WORKFLOW_PREFIX}${selection.workflowId ?? ""}` : NORMAL;
}

function selectionFromValue(value: string): components["schemas"]["PipelineSelection"] | null {
	if (value === UNSET) return null;
	if (value === NORMAL) return { mode: "normal_worker" };
	return { mode: "workflow", workflowId: value.slice(WORKFLOW_PREFIX.length) };
}

function collectDiagnostics(catalog: Catalog): Diagnostic[] {
	return [
		...(catalog.diagnostics ?? []),
		...(catalog.profiles ?? []).flatMap((p) => p.diagnostics ?? []),
		...(catalog.workflows ?? []).flatMap((w) => w.diagnostics ?? []),
	];
}

/**
 * Project-default pipeline selector. Definitions live in repository files, so
 * this surface only discovers, validates, and saves a reference; it never edits
 * a definition. Saving goes through its own endpoint and cannot replace other
 * project settings.
 */
export function ProjectPipelineSettings({ projectId, hostId }: { projectId: string; hostId?: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [showProblems, setShowProblems] = useState(false);
	const queryKey = projectPipelinesQueryKey(projectId, hostId);

	const query = useQuery({
		queryKey,
		queryFn: async () => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).GET("/api/v1/projects/{id}/pipelines", {
				params: { path: { id: projectId } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data as Catalog;
		},
	});

	const mutation = useMutation({
		mutationFn: async (value: string) => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).PUT("/api/v1/projects/{id}/pipelines/default", {
				params: { path: { id: projectId } },
				body: { selection: selectionFromValue(value) },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data as DefaultStatus;
		},
		onSuccess: (status) => {
			queryClient.setQueryData<Catalog>(queryKey, (current) => (current ? { ...current, default: status } : current));
		},
	});

	const trustMutation = useMutation({
		mutationFn: async (trusted: boolean) => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).PUT("/api/v1/projects/{id}/pipelines/command-trust", {
				params: { path: { id: projectId } },
				body: { trusted },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: (data) => {
			queryClient.setQueryData<Catalog>(queryKey, (current) => (current ? { ...current, commandsTrusted: data?.trusted ?? false } : current));
		},
	});

	const catalog = query.data;
	const declaresCommands = (catalog?.profiles ?? []).some((p) => (p.profile?.validation?.length ?? 0) > 0);
	const status = mutation.data ?? catalog?.default;
	const problems = catalog ? collectDiagnostics(catalog) : [];

	const options: SettingsOption<string>[] = [
		{ value: UNSET, label: t("settings.project.pipeline.unset") },
		{ value: NORMAL, label: t("settings.project.pipeline.normalWorker") },
	];
	for (const workflow of catalog?.workflows ?? []) {
		const suffix = !workflow.valid
			? t("settings.project.pipeline.invalidSuffix")
			: workflow.executable
				? ""
				: t("settings.project.pipeline.unavailableSuffix");
		options.push({
			value: `${WORKFLOW_PREFIX}${workflow.id}`,
			label: `${workflow.id}${suffix}`,
			disabled: !workflow.valid,
		});
	}
	const current = selectionValue(status);
	if (!options.some((option) => option.value === current)) {
		// A saved workflow that disappeared from the repository stays visible.
		options.push({ value: current, label: `${current.slice(WORKFLOW_PREFIX.length)}${t("settings.project.pipeline.missingSuffix")}`, disabled: true });
	}

	const attention = status && ["workflow_invalid", "workflow_missing"].includes(status.state);
	const note = status && status.state.startsWith("workflow_") && status.state !== "workflow_available" ? status.message : null;

	return (
		<ProjectSettingsSection title={t("settings.project.pipeline.title")} grouped>
			<div className="settings-row-bar">
				<span className="whitespace-nowrap text-sm leading-5 text-settings-label">{t("settings.project.pipeline.default")}</span>
				<div className="flex min-w-0 flex-1 items-center justify-end">
					{query.isLoading ? (
						<span className="text-sm text-settings-muted">{t("settings.project.pipeline.loading")}</span>
					) : query.isError ? (
						<span role="alert" className="text-sm text-error">
							{query.error instanceof Error ? query.error.message : t("settings.project.pipeline.loadFailed")}
						</span>
					) : (
						<SettingsOptionMenu
							aria-label={t("settings.project.pipeline.default")}
							value={current}
							options={options}
							disabled={mutation.isPending}
							onChange={(value) => mutation.mutate(value)}
						/>
					)}
				</div>
			</div>
			{declaresCommands ? (
				<div className="settings-row-bar">
					<span className="text-sm leading-5 text-settings-label">{t("settings.project.pipeline.trustCommands")}</span>
					<div className="flex min-w-0 flex-1 items-center justify-end">
						<Switch
							aria-label={t("settings.project.pipeline.trustCommands")}
							checked={catalog?.commandsTrusted ?? false}
							disabled={trustMutation.isPending}
							onCheckedChange={(checked) => trustMutation.mutate(checked)}
						/>
					</div>
				</div>
			) : null}
			{declaresCommands && !catalog?.commandsTrusted ? (
				<p className="text-pretty py-2 text-sm text-settings-muted">{t("settings.project.pipeline.trustCommandsHint")}</p>
			) : null}
			{trustMutation.isError ? (
				<p role="alert" className="text-pretty py-2 text-sm text-error">
					{trustMutation.error instanceof Error ? trustMutation.error.message : t("settings.project.pipeline.saveFailed")}
				</p>
			) : null}
			{note ? (
				<p role={attention ? "alert" : "status"} className={cn("text-pretty py-2 text-sm", attention ? "text-error" : "text-settings-muted")}>
					{note}
				</p>
			) : null}
			{mutation.isError ? (
				<p role="alert" className="text-pretty py-2 text-sm text-error">
					{mutation.error instanceof Error ? mutation.error.message : t("settings.project.pipeline.saveFailed")}
				</p>
			) : null}
			{problems.length > 0 ? (
				<div className="py-2">
					<button
						type="button"
						className="inline-flex items-center gap-1 text-sm text-settings-muted transition-colors hover:text-settings-label focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none"
						aria-expanded={showProblems}
						onClick={() => setShowProblems((open) => !open)}
					>
						{t("settings.project.pipeline.problems", { count: problems.length })}
						<ChevronDown className={cn("size-icon-sm transition-transform", showProblems && "rotate-180")} aria-hidden="true" />
					</button>
					{showProblems ? (
						<ul className="mt-2 flex flex-col gap-1.5 text-sm">
							{problems.map((problem, index) => (
								<li key={`${problem.file ?? ""}:${problem.field ?? ""}:${index}`} className="text-pretty">
									{problem.file ? <span className="font-mono text-xs text-settings-muted">{problem.file}{problem.field ? ` · ${problem.field}` : ""}</span> : null}
									<span className="block text-error">{problem.message}</span>
								</li>
							))}
						</ul>
					) : null}
				</div>
			) : null}
		</ProjectSettingsSection>
	);
}
