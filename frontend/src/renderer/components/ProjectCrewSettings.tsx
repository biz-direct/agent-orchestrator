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

type Catalog = components["schemas"]["CrewsCatalogResponse"];
type DefaultStatus = components["schemas"]["CrewsDefaultStatus"];
type Diagnostic = components["schemas"]["CrewDiagnostic"];

const UNSET = "unset";
const NO_CREW = "no-crew";
const WORKFLOW_PREFIX = "workflow:";

export const projectCrewQueryKey = (projectId: string, hostId?: string) =>
	hostId ? (["project-crew", hostId, projectId] as const) : (["project-crew", projectId] as const);

export function crewSelectionValue(status: DefaultStatus | undefined): string {
	const selection = status?.selection;
	if (!selection) return UNSET;
	return selection.mode === "workflow" ? `${WORKFLOW_PREFIX}${selection.workflowId ?? ""}` : NO_CREW;
}

function selectionFromValue(value: string): components["schemas"]["CrewSelection"] | null {
	if (value === UNSET) return null;
	if (value === NO_CREW) return { mode: "no_crew" };
	return { mode: "workflow", workflowId: value.slice(WORKFLOW_PREFIX.length) };
}

function collectDiagnostics(catalog: Catalog): Diagnostic[] {
	return [
		...(catalog.diagnostics ?? []),
		...(catalog.roles ?? []).flatMap((r) => r.diagnostics ?? []),
		...(catalog.workflows ?? []).flatMap((w) => w.diagnostics ?? []),
	];
}

/**
 * Project-default crew workflow selector. Definitions live in repository
 * files, so this surface only discovers, validates, and saves a reference; it
 * never edits a definition. Saving goes through its own endpoint and cannot
 * replace other project settings.
 */
export function ProjectCrewSettings({ projectId, hostId }: { projectId: string; hostId?: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [showProblems, setShowProblems] = useState(false);
	const queryKey = projectCrewQueryKey(projectId, hostId);

	const query = useQuery({
		queryKey,
		queryFn: async () => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).GET("/api/v1/projects/{id}/crew", {
				params: { path: { id: projectId } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data as Catalog;
		},
	});

	const mutation = useMutation({
		mutationFn: async (value: string) => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).PUT("/api/v1/projects/{id}/crew/default", {
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

	const catalog = query.data;
	const status = mutation.data ?? catalog?.default;
	const problems = catalog ? collectDiagnostics(catalog) : [];

	const options: SettingsOption<string>[] = [
		{ value: UNSET, label: t("settings.project.crew.unset") },
		{ value: NO_CREW, label: t("settings.project.crew.noCrew") },
	];
	for (const workflow of catalog?.workflows ?? []) {
		options.push({
			value: `${WORKFLOW_PREFIX}${workflow.id}`,
			label: `${workflow.id}${workflow.valid ? "" : t("settings.project.crew.invalidSuffix")}`,
			disabled: !workflow.valid,
		});
	}
	const current = crewSelectionValue(status);
	if (!options.some((option) => option.value === current)) {
		// A saved workflow that disappeared from the repository stays visible.
		options.push({ value: current, label: `${current.slice(WORKFLOW_PREFIX.length)}${t("settings.project.crew.missingSuffix")}`, disabled: true });
	}

	const attention = status && ["workflow_invalid", "workflow_missing"].includes(status.state);
	const note = attention ? status.message : null;

	return (
		<ProjectSettingsSection title={t("settings.project.crew.title")} grouped>
			<div className="settings-row-bar">
				<span className="whitespace-nowrap text-sm leading-5 text-settings-label">{t("settings.project.crew.default")}</span>
				<div className="flex min-w-0 flex-1 items-center justify-end">
					{query.isLoading ? (
						<span className="text-sm text-settings-muted">{t("settings.project.crew.loading")}</span>
					) : query.isError ? (
						<span role="alert" className="text-sm text-error">
							{query.error instanceof Error ? query.error.message : t("settings.project.crew.loadFailed")}
						</span>
					) : (
						<SettingsOptionMenu
							aria-label={t("settings.project.crew.default")}
							value={current}
							options={options}
							disabled={mutation.isPending}
							onChange={(value) => mutation.mutate(value)}
						/>
					)}
				</div>
			</div>
			{note ? (
				<p role="alert" className="text-pretty py-2 text-sm text-error">
					{note}
				</p>
			) : null}
			{mutation.isError ? (
				<p role="alert" className="text-pretty py-2 text-sm text-error">
					{mutation.error instanceof Error ? mutation.error.message : t("settings.project.crew.saveFailed")}
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
						{t("settings.project.crew.problems", { count: problems.length })}
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
