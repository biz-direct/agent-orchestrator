import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceSession } from "../types/workspace";
import { TooltipProvider } from "./ui/tooltip";

const { getMock, postMock, putMock, deleteMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn(), putMock: vi.fn(), deleteMock: vi.fn() }));

vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: () => ({ GET: getMock, POST: postMock, PUT: putMock, DELETE: deleteMock }),
}));
vi.mock("./chat/SessionChatSurface", () => ({
	SessionChatSurface: ({ session, sideChatsDisabled, assetBaseUrl, hostId, browserSession }: { session: { id: string; hostId?: string }; sideChatsDisabled?: boolean; assetBaseUrl?: string; hostId?: string; browserSession?: { id: string } }) => (
		<div data-browser-session={browserSession?.id ?? ""} data-session-host={session.hostId ?? ""} data-asset-base={assetBaseUrl ?? ""} data-host={hostId ?? ""} data-side-disabled={String(Boolean(sideChatsDisabled))} data-testid="role-chat">{session.id}</div>
	),
}));

import { AddCrewRoleMenu, CrewBatonMenu, CrewRoleChat, SessionRoleTab, useRemoveCrewRole, useSessionCrewRoles } from "./SessionCrewRoles";

const owner = { id: "w-1", workspaceId: "proj-1", workspaceName: "Proj", kind: "worker", provider: "claude-code" } as WorkspaceSession;
const builder = { sessionId: "w-att-2", ownerSessionId: "w-1", role: "builder", harness: "codex", controllerActive: true, createdAt: "2026-01-01T00:00:00Z" };

function wrap(ui: React.ReactNode) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>{ui}</TooltipProvider>
		</QueryClientProvider>,
	);
}

function Probe({ session }: { session: WorkspaceSession }) {
	const { eligible, roles } = useSessionCrewRoles(session);
	const remove = useRemoveCrewRole(session.id);
	return (
		<div>
			<span data-testid="eligible">{String(eligible)}</span>
			{roles.map((r) => (
				<button key={r.sessionId} onClick={() => remove.mutate(r.role)} type="button">
					drop {r.role}
				</button>
			))}
		</div>
	);
}

describe("crew role tabs", () => {
	beforeEach(() => {
		getMock.mockReset();
		postMock.mockReset();
		putMock.mockReset();
		deleteMock.mockReset().mockResolvedValue({ error: undefined });
	});

	it("lists a task's roles and removes only the chosen role", async () => {
		getMock.mockResolvedValue({ data: { roles: [builder] }, error: undefined });
		wrap(<Probe session={owner} />);
		await userEvent.click(await screen.findByRole("button", { name: "drop builder" }));
		await waitFor(() =>
			expect(deleteMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/roles/{role}", {
				params: { path: { sessionId: "w-1", role: "builder" } },
			}),
		);
	});

	it("never queries for orchestrators, ended tasks, or cloud tasks", () => {
		for (const session of [
			{ ...owner, kind: "orchestrator" },
			{ ...owner, isTerminated: true },
			{ ...owner, cloud: { orgId: "o" } },
		] as WorkspaceSession[]) {
			wrap(<Probe session={session} />);
		}
		expect(getMock).not.toHaveBeenCalled();
	});

	it("shows a role tab that selects and closes", async () => {
		const onActivate = vi.fn();
		const onClose = vi.fn();
		wrap(<SessionRoleTab active onActivate={onActivate} onClose={onClose} role={builder} />);
		await userEvent.click(screen.getByRole("tab", { name: "builder" }));
		await userEvent.click(screen.getByRole("button", { name: "Remove builder" }));
		expect(onActivate).toHaveBeenCalledTimes(1);
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it("adds a role the task does not have yet from the repository catalog and surfaces a refusal", async () => {
		getMock.mockResolvedValue({
			data: { roles: [{ id: "builder", valid: true }, { id: "tester", valid: true }, { id: "broken", valid: false }] },
			error: undefined,
		});
		postMock.mockResolvedValue({ error: { message: "harness x has no Chat driver" } });
		wrap(<AddCrewRoleMenu onAdded={vi.fn()} roles={[builder]} session={owner} />);
		await userEvent.click(await screen.findByRole("button", { name: "New role" }));
		expect(screen.queryByRole("menuitem", { name: "builder" })).not.toBeInTheDocument();
		expect(screen.queryByRole("menuitem", { name: "broken" })).not.toBeInTheDocument();
		await userEvent.click(await screen.findByRole("menuitem", { name: "tester" }));
		await waitFor(() =>
			expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/roles", {
				params: { path: { sessionId: "w-1" } },
				body: { role: "tester" },
			}),
		);
	});

	it("shows who holds the write baton and lets the person give or take it", async () => {
		getMock.mockResolvedValue({ data: { ownerSessionId: "w-1", holder: { kind: "worker" }, history: [] }, error: undefined });
		putMock.mockImplementation(async () => {
			getMock.mockResolvedValue({ data: { ownerSessionId: "w-1", holder: { kind: "role", role: "builder" }, history: [] }, error: undefined });
			return { data: { ownerSessionId: "w-1", holder: { kind: "role", role: "builder" }, history: [] }, error: undefined };
		});
		wrap(<CrewBatonMenu roles={[builder]} session={owner} />);
		await userEvent.click(await screen.findByRole("button", { name: "Write baton: worker" }));
		// The current holder cannot be picked again; every other holder can.
		expect(await screen.findByRole("menuitem", { name: "Give baton to worker" })).toHaveAttribute("aria-disabled", "true");
		await userEvent.click(screen.getByRole("menuitem", { name: "Give baton to builder" }));
		await waitFor(() =>
			expect(putMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/baton", {
				params: { path: { sessionId: "w-1" } },
				body: { to: { kind: "role", role: "builder" } },
			}),
		);
		expect(await screen.findByRole("button", { name: "Write baton: builder" })).toBeInTheDocument();
	});

	it("opens the role's own Chat conversation, never a terminal", () => {
		wrap(<CrewRoleChat owner={owner} role={builder} />);
		expect(screen.getByTestId("role-chat")).toHaveTextContent("w-att-2");
		// No side panel is mounted for a role tab, so side chats must not be offered.
		expect(screen.getByTestId("role-chat")).toHaveAttribute("data-side-disabled", "true");
	});

	it("gives a connected remote role the host's asset origin, and none while disconnected", () => {
		const { rerender } = wrap(<CrewRoleChat assetBaseUrl="http://127.0.0.1:9/proxy" hostId="remote-1" owner={owner} role={builder} />);
		expect(screen.getByTestId("role-chat")).toHaveAttribute("data-asset-base", "http://127.0.0.1:9/proxy");
		expect(screen.getByTestId("role-chat")).toHaveAttribute("data-host", "remote-1");
		rerender(
			<QueryClientProvider client={new QueryClient()}>
				<TooltipProvider>
					<CrewRoleChat hostId="remote-1" owner={owner} role={builder} />
				</TooltipProvider>
			</QueryClientProvider>,
		);
		expect(screen.getByTestId("role-chat")).toHaveAttribute("data-asset-base", "");
	});

	it("addresses messages to the role but opens browser links in the task's own pane", () => {
		const remoteOwner = { ...owner, hostId: "remote-1" } as WorkspaceSession;
		wrap(<CrewRoleChat hostId="remote-1" owner={remoteOwner} role={builder} />);
		const chat = screen.getByTestId("role-chat");
		expect(chat).toHaveTextContent("w-att-2");
		expect(chat).toHaveAttribute("data-browser-session", "w-1");
		expect(chat).toHaveAttribute("data-session-host", "remote-1");
	});
});
