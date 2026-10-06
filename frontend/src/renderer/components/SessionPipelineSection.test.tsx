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
	repairBudget: 3, repairsUsed: 0, repairsRemaining: 3, snapshotSha256: "x", snapshotCapturedAt: "now", events: [], evidence: [], revision: 1,
	createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
	stages: [{ id: "build", kind: "build", state: "active", model: "opus", settingsSource: "worker" }],
	attempts: [{ id: "a1", stageId: "build", attemptNo: 1, state: "active", executorSessionId: "w-1", noChange: false, instructionDelivery: "delivered", startedAt: "now" }],
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
					{ id: "a1", stageId: "build", attemptNo: 1, state: "accepted", executorSessionId: "w-1", noChange: false, instructionDelivery: "delivered", startedAt: "now" },
					{ id: "a2", stageId: "test", attemptNo: 1, state: "active", executorSessionId: "w-1-att-2", conversationSessionId: "w-1-att-2", noChange: false, instructionDelivery: "delivered", startedAt: "now" },
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
					{ id: "a2", stageId: "test", attemptNo: 1, state: "failed", outcome: "production_defect", executorSessionId: "w-1-att-2", noChange: false, instructionDelivery: "delivered", startedAt: "now",
						report: { findings: [{ criterion: "c", status: "unmet" }], commands: [{ command: "go test", exitCode: 1 }], remainingIssues: ["flaky"], defects: [{ description: "Add overflows" }] } },
				],
				evidence: [{ stageId: "test", attemptId: "a2", revision: "abcdef1234567", outcome: "production_defect", findings: 1, commands: 1, defects: 1, remainingIssues: 1 }],
			}),
			[],
		);
		renderSection();
		expect(await screen.findByText("May change: **/*_test.go, test/**")).toBeInTheDocument();
		expect(screen.getByText("Report: 1 findings, 1 commands, 1 remaining issues")).toBeInTheDocument();
		expect(screen.getAllByText("Add overflows").length).toBeGreaterThan(0);
		expect(screen.getByText("test: production defect at abcdef1")).toBeInTheDocument();
	});
});
