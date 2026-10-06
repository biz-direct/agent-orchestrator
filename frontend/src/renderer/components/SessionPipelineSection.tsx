import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InspectorSection as Section, inspectorEmptyClass } from "@aoagents/product-ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { cn } from "../lib/utils";
import type { WorkspaceSession } from "../types/workspace";
import { SettingsOptionMenu } from "./settings/SettingsOptionMenu";
import { Button } from "./ui/button";

type RunView = components["schemas"]["PipelineRunView"];
type StageView = components["schemas"]["PipelineStageView"];
type Catalog = components["schemas"]["PipelinesCatalogResponse"];

export const sessionPipelineQueryKey = (sessionId: string, hostId?: string) =>
	hostId ? (["session-pipeline", hostId, sessionId] as const) : (["session-pipeline", sessionId] as const);

const FINISHED_STATES = new Set(["completed", "cancelled"]);

function shortCommit(commit: string | undefined): string {
	return commit ? commit.slice(0, 7) : "";
}

/**
 * Pipeline state for one worker task, inside its existing task view: the run's
 * stages, the committed checkpoint, and any rejected hand-off. A task that has
 * no run and no startable workflow renders nothing, so ordinary workers look
 * exactly as before.
 */
export function SessionPipelineSection({ session, hostId }: { session: WorkspaceSession; hostId?: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [selected, setSelected] = useState<string | null>(null);
	const eligible = session.kind !== "orchestrator" && !session.cloud && session.isTerminated !== true;
	const client = clientForSessionHost(hostId);

	const runQuery = useQuery({
		queryKey: sessionPipelineQueryKey(session.id, hostId),
		enabled: eligible,
		// Unfinished runs change without a user action (a worker submits a result).
		refetchInterval: (query) => {
			const run = query.state.data?.run;
			return run && !FINISHED_STATES.has(run.state) ? 4000 : false;
		},
		queryFn: async () => {
			const { data, error } = await client.GET("/api/v1/sessions/{sessionId}/pipeline", { params: { path: { sessionId: session.id } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
	const run: RunView | null | undefined = runQuery.data?.run;
	const unfinished = run != null && !FINISHED_STATES.has(run.state);

	const catalogQuery = useQuery({
		queryKey: hostId ? ["project-pipelines", hostId, session.workspaceId] : ["project-pipelines", session.workspaceId],
		enabled: eligible && runQuery.isSuccess && !unfinished,
		queryFn: async () => {
			const { data, error } = await client.GET("/api/v1/projects/{id}/pipelines", { params: { path: { id: session.workspaceId } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data as Catalog;
		},
	});
	const startable = (catalogQuery.data?.workflows ?? []).filter((w) => w.valid && w.executable);
	const chosen = startable.find((w) => w.id === selected)?.id ?? startable[0]?.id ?? "";

	const start = useMutation({
		mutationFn: async (workflowId: string) => {
			const { data, error } = await client.POST("/api/v1/sessions/{sessionId}/pipeline", {
				params: { path: { sessionId: session.id } },
				body: { workflowId, requestedBy: "user" },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: (data) => {
			queryClient.setQueryData(sessionPipelineQueryKey(session.id, hostId), data);
		},
	});

	if (!eligible || runQuery.isLoading) return null;
	if (runQuery.isError) {
		return (
			<Section title={t("inspector.pipeline.title")}>
				<p className={inspectorEmptyClass} role="alert">
					{runQuery.error instanceof Error ? runQuery.error.message : t("inspector.pipeline.loadFailed")}
				</p>
			</Section>
		);
	}
	if (!run) {
		if (startable.length === 0) return null;
		return (
			<Section title={t("inspector.pipeline.title")}>
				<div className="flex items-center justify-between gap-2">
					<SettingsOptionMenu
						aria-label={t("inspector.pipeline.workflow")}
						value={chosen}
						options={startable.map((w) => ({ value: w.id, label: w.id }))}
						onChange={setSelected}
						disabled={start.isPending}
						menuAlign="start"
					/>
					<Button size="sm" variant="secondary" disabled={start.isPending || chosen === ""} onClick={() => start.mutate(chosen)}>
						{t("inspector.pipeline.start")}
					</Button>
				</div>
				{start.isError ? (
					<p className="mt-1.5 text-pretty text-2xs leading-normal text-error" role="alert">
						{start.error instanceof Error ? start.error.message : t("inspector.pipeline.startFailed")}
					</p>
				) : null}
			</Section>
		);
	}
	return (
		<Section title={t("inspector.pipeline.title")}>
			<RunSummary run={run} />
		</Section>
	);
}

function RunSummary({ run }: { run: RunView }) {
	const { t } = useTranslation();
	const paused = run.state === "paused";
	return (
		<div className="flex flex-col gap-2">
			<div className="flex items-baseline justify-between gap-2 text-sm">
				<span className="min-w-0 truncate font-medium">{run.workflowId}</span>
				<span className={cn("shrink-0 text-xs", paused ? "text-warning" : "text-settings-muted")}>{t(`inspector.pipeline.state.${run.state}`)}</span>
			</div>
			{paused && run.pauseDetail ? (
				<p className="text-pretty text-xs leading-normal text-warning" role="status">
					{run.pauseDetail}
				</p>
			) : null}
			<ul className="divide-y divide-(--color-border-settings-input)">
				{run.stages.map((stage) => (
					<StageRow key={stage.id} stage={stage} />
				))}
			</ul>
			{run.checkpoint ? (
				<p className="text-xs text-settings-muted">
					{run.checkpoint.noChange
						? t("inspector.pipeline.checkpointNoChange", { commit: shortCommit(run.checkpoint.outputCommit) })
						: t("inspector.pipeline.checkpoint", { from: shortCommit(run.checkpoint.inputCommit), to: shortCommit(run.checkpoint.outputCommit) })}
				</p>
			) : null}
			{run.lastRejection ? (
				<div className="text-xs leading-normal" role="alert">
					<p className="text-pretty text-error">{run.lastRejection.message}</p>
					{run.lastRejection.paths?.length ? (
						<ul className="mt-1 font-mono text-2xs text-settings-muted">
							{run.lastRejection.paths.map((path) => (
								<li key={path} className="truncate">
									{path}
								</li>
							))}
						</ul>
					) : null}
				</div>
			) : null}
		</div>
	);
}

function StageRow({ stage }: { stage: StageView }) {
	const { t } = useTranslation();
	return (
		<li className="flex items-baseline justify-between gap-2 py-1.5 text-sm" data-stage-state={stage.state}>
			<span className="min-w-0 truncate">
				{stage.id}
				{stage.model ? <span className="ml-1.5 text-xs text-settings-muted">{stage.model}</span> : null}
			</span>
			<span className={cn("shrink-0 text-xs", stage.state === "failed" ? "text-error" : stage.state === "accepted" ? "text-success" : "text-settings-muted")}>
				{t(`inspector.pipeline.stageState.${stage.state}`)}
			</span>
		</li>
	);
}
