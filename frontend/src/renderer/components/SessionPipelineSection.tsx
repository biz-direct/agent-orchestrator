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
import { StageConversationDialog } from "./StageConversationDialog";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./ui/button";

type RunView = components["schemas"]["PipelineRunView"];
type StageView = components["schemas"]["PipelineStageView"];
type AttemptView = components["schemas"]["PipelineAttemptView"];
type EvidenceView = components["schemas"]["PipelineEvidenceView"];
type CommandResult = components["schemas"]["PipelineCommandResultView"];
type ReviewGate = components["schemas"]["PipelineReviewGateView"];
type ReviewEvidence = components["schemas"]["PipelineReviewEvidenceView"];
type ControlRequest = components["schemas"]["PipelineControlRequest"];
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
			<RunSummary hostId={hostId} run={run} session={session} />
		</Section>
	);
}

function RunSummary({ run, session, hostId }: { run: RunView; session: WorkspaceSession; hostId?: string }) {
	const { t } = useTranslation();
	const [openStage, setOpenStage] = useState<{ stageId: string; sessionId: string; provider?: string } | null>(null);
	const paused = run.state === "paused";
	const conversationFor = (stageId: string) => {
		const attempts = run.attempts.filter((a) => a.stageId === stageId && a.conversationSessionId);
		return attempts[attempts.length - 1]?.conversationSessionId;
	};
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
			<RunControls run={run} session={session} hostId={hostId} />
			<ul className="divide-y divide-(--color-border-settings-input)">
				{run.stages.map((stage) => {
					const conversation = conversationFor(stage.id);
					return (
						<StageRow
							key={stage.id}
							stage={stage}
							report={latestReport(run.attempts, stage.id)}
							validation={latestValidation(run.attempts, stage.id)}
							onOpenConversation={conversation ? () => setOpenStage({ stageId: stage.id, sessionId: conversation, provider: stage.harness }) : undefined}
						/>
					);
				})}
			</ul>
			{run.repairBudget > 0 || run.repairs.length > 0 ? (
				<p className="text-xs text-settings-muted" data-testid="repair-budget">
					{t("inspector.pipeline.repairs", { used: run.repairsUsed, budget: run.repairBudget, remaining: run.repairsRemaining })}
				</p>
			) : null}
			{run.repairs.length > 0 ? (
				<ul className="flex flex-col gap-0.5 text-xs text-settings-muted">
					{run.repairs.map((repair) => (
						<li key={repair.ordinal} className="text-pretty">
							{t("inspector.pipeline.repairItem", {
								ordinal: repair.ordinal,
								source: repair.sourceStageId,
								target: repair.targetStageId,
								kind: t(`inspector.pipeline.repairKind.${repair.kind}`, { defaultValue: repair.kind }),
							})}
						</li>
					))}
				</ul>
			) : null}
			{run.reviewGate ? <ReviewGatePanel gate={run.reviewGate} /> : null}
			{run.reviews.length > 0 ? <ReviewEvidenceList reviews={run.reviews} /> : null}
			{run.evidence.length > 0 ? <EvidenceList evidence={run.evidence} /> : null}
			{run.checkpoint ? (
				<p className="text-xs text-settings-muted">
					{run.checkpoint.noChange
						? t("inspector.pipeline.checkpointNoChange", { commit: shortCommit(run.checkpoint.outputCommit) })
						: t("inspector.pipeline.checkpoint", { from: shortCommit(run.checkpoint.inputCommit), to: shortCommit(run.checkpoint.outputCommit) })}
				</p>
			) : null}
			{openStage ? (
				<StageConversationDialog
					conversationSessionId={openStage.sessionId}
					stageId={openStage.stageId}
					owner={session}
					hostId={hostId}
					provider={openStage.provider}
					onClose={() => setOpenStage(null)}
				/>
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

function latestReport(attempts: AttemptView[], stageId: string) {
	const withReport = attempts.filter((a) => a.stageId === stageId && a.report);
	return withReport[withReport.length - 1]?.report;
}

function latestValidation(attempts: AttemptView[], stageId: string): CommandResult[] {
	const withChecks = attempts.filter((a) => a.stageId === stageId && a.validation.length > 0);
	const latest = withChecks[withChecks.length - 1]?.validation ?? [];
	const round = Math.max(0, ...latest.map((c) => c.round));
	return latest.filter((c) => c.round === round);
}

function ValidationList({ checks }: { checks: CommandResult[] }) {
	const { t } = useTranslation();
	return (
		<ul className="mt-1 flex flex-col gap-0.5 text-2xs leading-normal">
			{checks.map((check) => {
				const bad = check.status !== "passed" && check.status !== "skipped";
				return (
					<li key={`${check.round}-${check.commandId}`} data-check-status={check.status}>
						<div className="flex items-baseline justify-between gap-2">
							<span className="min-w-0 truncate font-mono">{check.commandId}</span>
							<span className={cn("shrink-0", check.status === "passed" ? "text-success" : bad ? "text-error" : "text-settings-muted")}>
								{t(`inspector.pipeline.check.${check.status}`)}
								{check.status === "failed" ? ` (${check.exitCode})` : ""} · {check.revision.slice(0, 7)}
							</span>
						</div>
						{bad && check.detail ? <p className="text-pretty text-settings-muted">{check.detail}</p> : null}
						{bad && check.log ? (
							<details>
								<summary className="cursor-pointer text-settings-muted">{t("inspector.pipeline.checkOutput")}</summary>
								<pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words font-mono text-2xs">{check.log}</pre>
							</details>
						) : null}
					</li>
				);
			})}
		</ul>
	);
}

/**
 * Pause, resume, and cancel for an unfinished run. The daemon decides what is
 * allowed (`run.control`); this only offers it. Extra repairs after the budget
 * is spent are a separate, explicit authorization: resume never grants them.
 */
function RunControls({ run, session, hostId }: { run: RunView; session: WorkspaceSession; hostId?: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const client = clientForSessionHost(hostId);
	const [confirm, setConfirm] = useState<"cancel" | "authorize" | "restore" | null>(null);
	const control = useMutation({
		mutationFn: async (input: Pick<ControlRequest, "action" | "additionalRepairs" | "requestKey" | "recovery">) => {
			const { data, error } = await client.POST("/api/v1/sessions/{sessionId}/pipeline/control", {
				params: { path: { sessionId: session.id } },
				body: { runId: run.id, requestedBy: "user", expectedRevision: run.revision, ...input },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: (data) => {
			queryClient.setQueryData(sessionPipelineQueryKey(session.id, hostId), { run: data.run });
			setConfirm(null);
		},
	});
	const { control: state } = run;
	const lastStop = state.lastStop;
	const canRestore = run.state === "paused" && state.recoveryOptions.includes("restore_conversation");
	if (!state.canPause && !state.canResume && !state.canCancel && !state.needsRepairAuthorization && !canRestore && run.repairGrants.length === 0) return null;
	return (
		<div className="flex flex-col gap-1.5" data-testid="pipeline-controls">
			<div className="flex flex-wrap items-center gap-1.5">
				{state.canPause ? (
					<Button size="sm" variant="secondary" disabled={control.isPending} onClick={() => control.mutate({ action: "pause" })}>
						{t("inspector.pipeline.control.pause")}
					</Button>
				) : null}
				{state.canResume ? (
					<Button size="sm" variant="secondary" disabled={control.isPending} onClick={() => control.mutate({ action: "resume" })}>
						{t("inspector.pipeline.control.resume")}
					</Button>
				) : null}
				{canRestore ? (
					<Button size="sm" variant="secondary" disabled={control.isPending} onClick={() => setConfirm("restore")}>
						{t("inspector.pipeline.control.restore")}
					</Button>
				) : null}
				{state.needsRepairAuthorization ? (
					<Button size="sm" variant="secondary" disabled={control.isPending} onClick={() => setConfirm("authorize")}>
						{t("inspector.pipeline.control.authorize")}
					</Button>
				) : null}
				{state.canCancel ? (
					<Button size="sm" variant="ghost" disabled={control.isPending} onClick={() => setConfirm("cancel")}>
						{t("inspector.pipeline.control.cancel")}
					</Button>
				) : null}
			</div>
			{run.state === "paused" && state.resumeNeedsUser && !state.needsRepairAuthorization ? (
				<p className="text-pretty text-2xs leading-normal text-settings-muted">{t("inspector.pipeline.control.humanOnly")}</p>
			) : null}
			{lastStop ? (
				<p className={cn("text-pretty text-2xs leading-normal", lastStop.confirmed ? "text-settings-muted" : "text-warning")} role="status">
					{lastStop.confirmed ? t("inspector.pipeline.control.stopConfirmed") : t("inspector.pipeline.control.stopUnconfirmed", { detail: lastStop.detail ?? "" })}
				</p>
			) : null}
			{state.lastRecovery ? (
				<p className="text-pretty text-2xs leading-normal text-settings-muted" data-recovery-outcome={state.lastRecovery.outcome}>
					{t("inspector.pipeline.control.recovery", { message: state.lastRecovery.message })}
				</p>
			) : null}
			{run.repairGrants.length > 0 ? (
				<p className="text-2xs text-settings-muted">{t("inspector.pipeline.control.grants", { count: run.repairGrants.reduce((sum, g) => sum + g.amount, 0) })}</p>
			) : null}
			{control.isError ? (
				<p className="text-pretty text-2xs leading-normal text-error" role="alert">
					{control.error instanceof Error ? control.error.message : t("inspector.pipeline.control.failed")}
				</p>
			) : null}
			<ConfirmDialog
				open={confirm === "cancel"}
				title={t("inspector.pipeline.control.cancelTitle")}
				description={t("inspector.pipeline.control.cancelDescription")}
				confirmLabel={t("inspector.pipeline.control.cancelConfirm")}
				cancelLabel={t("inspector.pipeline.control.cancelKeep")}
				destructive
				busy={control.isPending}
				error={control.isError ? (control.error instanceof Error ? control.error.message : t("inspector.pipeline.control.failed")) : null}
				onConfirm={() => control.mutate({ action: "cancel" })}
				onOpenChange={(open) => !open && setConfirm(null)}
			/>
			<ConfirmDialog
				open={confirm === "restore"}
				title={t("inspector.pipeline.control.restoreTitle")}
				description={t("inspector.pipeline.control.restoreDescription")}
				confirmLabel={t("inspector.pipeline.control.restoreConfirm")}
				busy={control.isPending}
				error={control.isError ? (control.error instanceof Error ? control.error.message : t("inspector.pipeline.control.failed")) : null}
				onConfirm={() => control.mutate({ action: "resume", recovery: "restore_conversation" })}
				onOpenChange={(open) => !open && setConfirm(null)}
			/>
			<ConfirmDialog
				open={confirm === "authorize"}
				title={t("inspector.pipeline.control.authorizeTitle")}
				description={t("inspector.pipeline.control.authorizeDescription")}
				confirmLabel={t("inspector.pipeline.control.authorizeConfirm")}
				busy={control.isPending}
				error={control.isError ? (control.error instanceof Error ? control.error.message : t("inspector.pipeline.control.failed")) : null}
				onConfirm={() => control.mutate({ action: "authorize_repairs", additionalRepairs: 1, requestKey: `ui-${run.id}-${run.revision}` })}
				onOpenChange={(open) => !open && setConfirm(null)}
			/>
		</div>
	);
}

/**
 * Current-head review readiness: what AO is waiting for, or what a person has
 * to do. Review stays AO's existing reviewer; this only reports where the run
 * stands for the revision under review, and never implies a merge.
 */
function ReviewGatePanel({ gate }: { gate: ReviewGate }) {
	const { t } = useTranslation();
	return (
		<div className="flex flex-col gap-0.5 text-xs" data-testid="review-gate" data-review-gate-state={gate.state} data-review-gate-code={gate.code}>
			<div className="flex items-baseline justify-between gap-2">
				<span className="font-medium">{t("inspector.pipeline.reviewGate.title")}</span>
				<span className={cn("shrink-0", gate.state === "ready" ? "text-success" : gate.state === "blocked" ? "text-warning" : "text-settings-muted")}>
					{t(`inspector.pipeline.reviewGate.state.${gate.state}`)}
				</span>
			</div>
			<p className="text-pretty text-settings-muted" role="status">
				{t(`inspector.pipeline.reviewGate.code.${gate.code}`, { defaultValue: gate.message })}
			</p>
			{gate.manual ? <p className="text-pretty text-settings-muted">{t("inspector.pipeline.reviewGate.manual")}</p> : null}
			<p className="text-2xs text-settings-muted">{t("inspector.pipeline.reviewGate.revision", { revision: shortCommit(gate.checkpoint) })}</p>
			{gate.headSha ? <p className="text-2xs text-settings-muted">{t("inspector.pipeline.reviewGate.head", { revision: shortCommit(gate.headSha) })}</p> : null}
			{gate.reviewStatus ? (
				<p className="text-2xs text-settings-muted">
					{gate.verdict
						? t("inspector.pipeline.reviewGate.reviewRunVerdict", { status: gate.reviewStatus, verdict: gate.verdict.replace("_", " ") })
						: t("inspector.pipeline.reviewGate.reviewRun", { status: gate.reviewStatus })}
				</p>
			) : null}
			{gate.ci ? (
				<p className="text-pretty text-2xs text-settings-muted" title={gate.ciDetail}>
					{t("inspector.pipeline.reviewGate.ci", { state: t(`inspector.pipeline.reviewGate.ciState.${gate.ci}`, { defaultValue: gate.ci }) })}
				</p>
			) : null}
		</div>
	);
}

function ReviewEvidenceList({ reviews }: { reviews: ReviewEvidence[] }) {
	const { t } = useTranslation();
	return (
		<ul className="flex flex-col gap-0.5 text-xs text-settings-muted">
			{reviews.map((item) => (
				<li key={item.attemptId} className="text-pretty">
					{t(item.current ? "inspector.pipeline.reviewEvidence" : "inspector.pipeline.reviewEvidenceSuperseded", {
						revision: shortCommit(item.headSha || item.revision),
						outcome: t(`inspector.pipeline.outcome.${item.outcome ?? ""}`, { defaultValue: item.outcome || t("inspector.pipeline.stageState.interrupted") }),
					})}
				</li>
			))}
		</ul>
	);
}

function EvidenceList({ evidence }: { evidence: EvidenceView[] }) {
	const { t } = useTranslation();
	return (
		<ul className="flex flex-col gap-0.5 text-xs text-settings-muted">
			{evidence.map((item) => (
				<li key={item.attemptId} className="text-pretty">
					{t(item.current ? "inspector.pipeline.evidence" : "inspector.pipeline.evidenceSuperseded", {
						stage: item.stageId,
						outcome: t(`inspector.pipeline.outcome.${item.outcome}`, { defaultValue: item.outcome }),
						revision: item.revision.slice(0, 7),
					})}
				</li>
			))}
		</ul>
	);
}

function StageRow({ stage, report, validation, onOpenConversation }: { stage: StageView; report?: AttemptView["report"]; validation: CommandResult[]; onOpenConversation?: () => void }) {
	const { t } = useTranslation();
	return (
		<li className="py-1.5 text-sm" data-stage-state={stage.state}>
			<div className="flex items-baseline justify-between gap-2">
			<span className="min-w-0 truncate">
				{onOpenConversation ? (
					<button
						type="button"
						className="text-left underline-offset-2 hover:underline focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none"
						aria-label={t("inspector.pipeline.openConversation", { stage: stage.id })}
						onClick={onOpenConversation}
					>
						{stage.id}
					</button>
				) : (
					stage.id
				)}
				{stage.model ? <span className="ml-1.5 text-xs text-settings-muted">{stage.model}</span> : null}
			</span>
			<span className={cn("shrink-0 text-xs", stage.state === "failed" ? "text-error" : stage.state === "accepted" ? "text-success" : "text-settings-muted")}>
				{t(`inspector.pipeline.stageState.${stage.state}`)}
			</span>
			</div>
			{stage.allowedPaths?.length ? (
				<p className="mt-0.5 text-pretty text-2xs leading-normal text-settings-muted" title={t("inspector.pipeline.scopeNote")}>
					{t("inspector.pipeline.scope", { paths: stage.allowedPaths.join(", ") })}
				</p>
			) : null}
			{validation.length > 0 ? <ValidationList checks={validation} /> : null}
			{report ? (
				<div className="mt-0.5 text-pretty text-2xs leading-normal text-settings-muted">
					<p>
						{t("inspector.pipeline.reportSummary", {
							findings: report.findings.length,
							commands: report.commands.length,
							issues: report.remainingIssues.length,
						})}
					</p>
					{report.defects.map((defect) => (
						<p key={defect.description} className="text-error">
							{defect.description}
						</p>
					))}
				</div>
			) : null}
		</li>
	);
}
