import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceSession } from "../types/workspace";
import { TooltipProvider } from "./ui/tooltip";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/host-clients", () => ({ clientForSessionHost: () => ({ GET: getMock, POST: postMock }) }));
vi.mock("../lib/api-client", () => ({
	apiErrorMessage: (error: unknown) =>
		typeof error === "object" && error !== null && "message" in error ? String((error as { message: unknown }).message) : "Request failed",
}));

vi.mock("./chat/SessionChatSurface", () => ({
	SessionChatSurface: ({ session }: { session: { id: string } }) => <div data-testid="stage-chat">{session.id}</div>,
}));

import { SessionPipelineSection } from "./SessionPipelineSection";

const session = { id: "w-1", workspaceId: "proj", workspaceName: "proj", title: "t", provider: "claude-code", status: "working" } as unknown as WorkspaceSession;

const catalog = (workflows: unknown[]) => ({ projectId: "proj", profiles: [], workflows, diagnostics: [], default: { selection: null, state: "unset", executable: false, message: "" } });
const buildOnly = { id: "build-only", file: "f", valid: true, executable: true, diagnostics: [] };
const multi = { id: "build-test", file: "f", valid: true, executable: false, unavailableReason: "later", diagnostics: [] };

const run = (overrides: Record<string, unknown> = {}) => ({
	id: "prun_1", sessionId: "w-1", projectId: "proj", workflowId: "build-only", state: "running", currentStageId: "build", requestedBy: "user",
	repairBudget: 3, repairsUsed: 0, repairsRemaining: 3, snapshotSha256: "x", snapshotCapturedAt: "now", events: [], evidence: [], repairs: [], reviews: [], repairGrants: [], revision: 1,
	control: { canPause: true, canResume: false, canCancel: true, resumeNeedsUser: false, needsRepairAuthorization: false },
	createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
	stages: [{ id: "build", kind: "build", state: "active", model: "opus", settingsSource: "worker" }],
	attempts: [{ id: "a1", stageId: "build", attemptNo: 1, state: "active", executorSessionId: "w-1", noChange: false, instructionDelivery: "delivered", startedAt: "now", validation: [] }],
	...overrides,
});

function renderSection(s: WorkspaceSession = session) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<SessionPipelineSection session={s} />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

function mockGets(runBody: unknown, workflows: unknown[] = [buildOnly]) {
	getMock.mockImplementation(async (path: string) => {
		if (path === "/api/v1/sessions/{sessionId}/pipeline") return { data: { run: runBody } };
		return { data: catalog(workflows) };
	});
}

describe("SessionPipelineSection", () => {
	beforeEach(() => {
		getMock.mockReset();
		postMock.mockReset();
	});

	it("renders nothing for an ordinary worker with no startable workflow", async () => {
		mockGets(null, [multi]);
		const { container } = renderSection();
		await waitFor(() => expect(getMock).toHaveBeenCalledTimes(2));
		expect(container).toBeEmptyDOMElement();
	});

	it("renders nothing for orchestrators and never queries", () => {
		mockGets(null);
		const { container } = renderSection({ ...session, kind: "orchestrator" } as WorkspaceSession);
		expect(container).toBeEmptyDOMElement();
		expect(getMock).not.toHaveBeenCalled();
	});

	it("starts an executable workflow explicitly as the user", async () => {
		mockGets(null);
		postMock.mockResolvedValue({ data: { run: run() } });
		renderSection();
		await userEvent.click(await screen.findByRole("button", { name: "Start pipeline" }));
		await waitFor(() =>
			expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/pipeline", {
				params: { path: { sessionId: "w-1" } },
				body: { workflowId: "build-only", requestedBy: "user" },
			}),
		);
		expect(await screen.findByText("Running")).toBeInTheDocument();
	});

	it("surfaces start failures next to the control", async () => {
		mockGets(null);
		postMock.mockResolvedValue({ error: { message: "This task already has an unfinished pipeline run" } });
		renderSection();
		await userEvent.click(await screen.findByRole("button", { name: "Start pipeline" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("already has an unfinished pipeline run");
	});

	it("shows stages and the committed checkpoint of a finished run", async () => {
		mockGets(
			run({
				state: "completed",
				stages: [{ id: "build", kind: "build", state: "accepted", model: "opus", settingsSource: "worker" }],
				checkpoint: { stageId: "build", inputCommit: "aaaaaaa1111", outputCommit: "bbbbbbb2222", noChange: false },
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText("Complete")).toBeInTheDocument();
		expect(screen.getByText("Accepted")).toBeInTheDocument();
		expect(screen.getByText("Checkpoint aaaaaaa → bbbbbbb")).toBeInTheDocument();
	});

	it("explains a rejected dirty hand-off with the offending paths", async () => {
		mockGets(
			run({ lastRejection: { code: "PIPELINE_DIRTY_WORKSPACE", message: "The workspace has 1 uncommitted path(s).", paths: ["?? wip.txt"], at: "now" } }),
			[],
		);
		renderSection();
		expect(await screen.findByRole("alert")).toHaveTextContent("uncommitted path");
		expect(screen.getByText("?? wip.txt")).toBeInTheDocument();
	});

	it("makes a pause visible with its reason", async () => {
		mockGets(
			run({
				state: "paused",
				pauseReason: "controller_changed",
				pauseDetail: "The worker's controller restarted while the stage was active",
				stages: [{ id: "build", kind: "build", state: "paused", settingsSource: "worker" }],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByRole("status")).toHaveTextContent("controller restarted");
		expect(screen.getAllByText("Paused")).toHaveLength(2);
	});

	it("shows a handoff in flight and opens a finished or running stage's own conversation", async () => {
		mockGets(
			run({
				currentStageId: "test",
				stages: [
					{ id: "build", kind: "build", state: "accepted", settingsSource: "worker" },
					{ id: "test", kind: "specialist", state: "active", harness: "claude-code", settingsSource: "profile" },
				],
				attempts: [
					{ id: "a1", stageId: "build", attemptNo: 1, state: "accepted", executorSessionId: "w-1", noChange: false, instructionDelivery: "delivered", startedAt: "now", validation: [] },
					{ id: "a2", stageId: "test", attemptNo: 1, state: "active", executorSessionId: "w-1-att-2", conversationSessionId: "w-1-att-2", noChange: false, instructionDelivery: "delivered", startedAt: "now", validation: [] },
				],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText("Accepted")).toBeInTheDocument();
		// Build ran in the task's own conversation; only the specialist has a dialog.
		expect(screen.queryByRole("button", { name: "Open build conversation" })).not.toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Open test conversation" }));
		expect(await screen.findByTestId("stage-chat")).toHaveTextContent("w-1-att-2");
	});

	it("labels a stage that is waiting for its handoff", async () => {
		mockGets(
			run({
				currentStageId: "test",
				stages: [
					{ id: "build", kind: "build", state: "accepted", settingsSource: "worker" },
					{ id: "test", kind: "specialist", state: "handoff", settingsSource: "profile" },
				],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText("Handing off")).toBeInTheDocument();
	});

	it("labels specialist evidence with the revision it covers and shows scope and report", async () => {
		mockGets(
			run({
				state: "paused",
				pauseReason: "production_defect",
				pauseDetail: "Stage test found 1 production defect(s) for Build to fix",
				currentStageId: "test",
				stages: [
					{ id: "build", kind: "build", state: "accepted", settingsSource: "worker" },
					{ id: "test", kind: "specialist", state: "failed", settingsSource: "profile", allowedPaths: ["**/*_test.go", "test/**"] },
				],
				attempts: [
					{ id: "a2", stageId: "test", attemptNo: 1, state: "failed", outcome: "production_defect", executorSessionId: "w-1-att-2", noChange: false, instructionDelivery: "delivered", startedAt: "now", validation: [],
						report: { findings: [{ criterion: "c", status: "unmet" }], commands: [{ command: "go test", exitCode: 1 }], remainingIssues: ["flaky"], defects: [{ description: "Add overflows" }] } },
				],
				evidence: [{ stageId: "test", attemptId: "a2", revision: "abcdef1234567", outcome: "production_defect", findings: 1, commands: 1, defects: 1, remainingIssues: 1, current: true }],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText("May change: **/*_test.go, test/**")).toBeInTheDocument();
		expect(screen.getByText("Report: 1 findings, 1 commands, 1 remaining issues")).toBeInTheDocument();
		expect(screen.getAllByText("Add overflows").length).toBeGreaterThan(0);
		expect(screen.getByText("test: production defect at abcdef1")).toBeInTheDocument();
	});

	it("shows AO's own check results bound to the revision, with failing output", async () => {
		mockGets(
			run({
				state: "paused",
				pauseReason: "validation_failed",
				pauseDetail: "Mandatory validation failed against abcdef1: unit (exit 3)",
				currentStageId: "test",
				stages: [
					{ id: "build", kind: "build", state: "accepted", settingsSource: "worker" },
					{ id: "test", kind: "specialist", state: "failed", settingsSource: "profile" },
				],
				attempts: [
					{ id: "a2", stageId: "test", attemptNo: 1, state: "failed", executorSessionId: "w-1-att-2", noChange: false, instructionDelivery: "delivered", startedAt: "now",
						validation: [
							{ round: 1, commandId: "unit", command: "go test", required: true, revision: "abcdef1234567", status: "failed", exitCode: 3, startedAt: "now", durationMs: 5, log: "FAIL: TestX", logTruncated: false },
							{ round: 1, commandId: "lint", command: "lint", required: true, revision: "abcdef1234567", status: "skipped", exitCode: 0, startedAt: "now", durationMs: 0, logTruncated: false },
						] },
				],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText(/failed \(3\) · abcdef1/)).toBeInTheDocument();
		expect(screen.getByText("skipped · abcdef1")).toBeInTheDocument();
		expect(screen.getByText("FAIL: TestX")).toBeInTheDocument();
	});

	it("shows the shared repair budget, each return, and marks superseded evidence", async () => {
		mockGets(
			run({
				repairsUsed: 1,
				repairsRemaining: 2,
				repairs: [{ ordinal: 1, kind: "production_defect", sourceStageId: "test", sourceAttemptId: "a2", targetStageId: "build", returnStageId: "test", createdAt: "now" }],
				evidence: [
					{ stageId: "test", attemptId: "a2", revision: "aaaaaaa1111", outcome: "production_defect", findings: 1, commands: 0, defects: 1, remainingIssues: 0, current: false },
					{ stageId: "test", attemptId: "a4", revision: "bbbbbbb2222", outcome: "succeeded", findings: 1, commands: 1, defects: 0, remainingIssues: 0, current: true },
				],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByTestId("repair-budget")).toHaveTextContent("Repairs: 1 of 3 used, 2 remaining");
		expect(screen.getByText("Return 1: test → build (production defect)")).toBeInTheDocument();
		expect(screen.getByText("test: production defect at aaaaaaa (a newer commit exists; this no longer applies)")).toBeInTheDocument();
		expect(screen.getByText("test: passed at bbbbbbb")).toBeInTheDocument();
	});
	it("shows manual review waiting for the current head without implying a merge", async () => {
		mockGets(
			run({
				currentStageId: "review",
				stages: [
					{ id: "build", kind: "build", state: "accepted", settingsSource: "worker" },
					{ id: "review", kind: "review", state: "active", settingsSource: "reviewer" },
				],
				reviewGate: {
					state: "waiting", code: "awaiting_manual_review", message: "Auto review is off", manual: true, autoReview: false,
					checkpoint: "aaaaaaaaaaaaaaaa", headSha: "aaaaaaaaaaaaaaaa", prUrl: "https://github.com/o/r/pull/1", ci: "no_checks", ciDetail: "none",
				},
			}),
		);
		renderSection();
		const gate = await screen.findByTestId("review-gate");
		expect(gate).toHaveAttribute("data-review-gate-state", "waiting");
		expect(gate).toHaveTextContent("Waiting for a manual review");
		expect(gate).toHaveTextContent("Auto review is off. Trigger the review from this task's review controls.");
		expect(gate).toHaveTextContent("Reviewing revision aaaaaaa");
		expect(gate).toHaveTextContent("Required checks: none to wait for");
	});

	it("explains a blocked review and keeps earlier review evidence labelled by revision", async () => {
		mockGets(
			run({
				state: "paused", pauseReason: "review_changes_requested", pauseDetail: "The built-in review requested changes on revision aaaaaaaaaa.",
				currentStageId: "review",
				stages: [{ id: "review", kind: "review", state: "paused", settingsSource: "reviewer" }],
				reviewGate: {
					state: "blocked", code: "review_changes_requested", message: "x", manual: false, autoReview: true,
					checkpoint: "aaaaaaaaaaaaaaaa", headSha: "aaaaaaaaaaaaaaaa", reviewRunId: "rr1", reviewStatus: "complete", verdict: "changes_requested",
				},
				reviews: [
					{ stageId: "review", attemptId: "a9", revision: "bbbbbbbbbbbb", headSha: "bbbbbbbbbbbb", outcome: "changes_requested", current: false },
				],
			}),
		);
		renderSection();
		const gate = await screen.findByTestId("review-gate");
		expect(gate).toHaveAttribute("data-review-gate-state", "blocked");
		expect(gate).toHaveTextContent("The review requested changes");
		expect(gate).toHaveTextContent("AO review: complete, changes requested");
		expect(screen.getByText(/Review of bbbbbbb: changes requested \(a newer revision exists/)).toBeInTheDocument();
	});
	it("pauses a running pipeline as the user and shows an unconfirmed stop honestly", async () => {
		mockGets(run());
		postMock.mockResolvedValue({
			data: {
				changed: true,
				stop: { requested: true, confirmed: false, detail: "waiting on a permission request" },
				run: run({
					state: "paused", pauseReason: "paused_by_user", pauseDetail: "Paused by the user", revision: 2,
					control: { canPause: false, canResume: true, canCancel: true, resumeNeedsUser: false, needsRepairAuthorization: false, lastStop: { requested: true, confirmed: false, detail: "waiting on a permission request" } },
				}),
			},
		});
		renderSection();
		await userEvent.click(await screen.findByRole("button", { name: "Pause" }));
		await waitFor(() =>
			expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/pipeline/control", {
				params: { path: { sessionId: "w-1" } },
				body: { runId: "prun_1", requestedBy: "user", expectedRevision: 1, action: "pause" },
			}),
		);
		expect(await screen.findByRole("button", { name: "Resume" })).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Pause" })).not.toBeInTheDocument();
		expect(screen.getByText("Stop requested but not confirmed: waiting on a permission request")).toBeInTheDocument();
	});

	it("asks before cancelling and explains that nothing is deleted", async () => {
		mockGets(run());
		postMock.mockResolvedValue({ data: { changed: true, run: run({ state: "cancelled", control: { canPause: false, canResume: false, canCancel: false, resumeNeedsUser: false, needsRepairAuthorization: false } }) } });
		renderSection();
		await userEvent.click(await screen.findByRole("button", { name: "Cancel pipeline" }));
		expect(await screen.findByText(/Nothing is reset or deleted/)).toBeInTheDocument();
		expect(postMock).not.toHaveBeenCalled();
		await userEvent.click(screen.getAllByRole("button", { name: "Cancel pipeline" }).at(-1) as HTMLElement);
		await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/pipeline/control", expect.objectContaining({ body: expect.objectContaining({ action: "cancel", requestedBy: "user" }) })));
	});

	it("offers an explicit repair authorization instead of resume once the budget is spent", async () => {
		mockGets(
			run({
				state: "paused", pauseReason: "repair_budget_exhausted", pauseDetail: "All 3 repairs used", repairsUsed: 3, repairsRemaining: 0,
				control: { canPause: false, canResume: false, canCancel: true, resumeNeedsUser: true, needsRepairAuthorization: true },
			}),
		);
		postMock.mockResolvedValue({
			data: {
				changed: true,
				run: run({
					state: "paused", pauseReason: "repair_budget_exhausted", repairBudget: 4, repairsUsed: 3, repairsRemaining: 1, revision: 2,
					repairGrants: [{ amount: 1, authorizedBy: "user", createdAt: "2026-01-01T00:00:00Z" }],
					control: { canPause: false, canResume: true, canCancel: true, resumeNeedsUser: true, needsRepairAuthorization: false },
				}),
			},
		});
		renderSection();
		expect(await screen.findByRole("button", { name: "Authorize repair" })).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Authorize repair" }));
		expect(await screen.findByText(/resuming never adds attempts/)).toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Authorize one repair" }));
		await waitFor(() =>
			expect(postMock).toHaveBeenCalledWith(
				"/api/v1/sessions/{sessionId}/pipeline/control",
				expect.objectContaining({ body: expect.objectContaining({ action: "authorize_repairs", additionalRepairs: 1, requestedBy: "user" }) }),
			),
		);
		expect(await screen.findByRole("button", { name: "Resume" })).toBeInTheDocument();
		expect(screen.getByText("Extra repairs authorized by you: 1")).toBeInTheDocument();
	});
});
