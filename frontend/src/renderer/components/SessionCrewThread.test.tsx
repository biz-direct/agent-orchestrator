import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceSession } from "../types/workspace";

const { getMock, patchMock, deleteMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn(), deleteMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: () => ({ GET: getMock, PATCH: patchMock, DELETE: deleteMock, POST: postMock }),
}));

import { CrewThreadPane } from "./SessionCrewThread";

const owner = { id: "w-1", workspaceId: "proj-1", kind: "worker" } as WorkspaceSession;

const message = (over: Record<string, unknown>) => ({
	id: "m-1",
	seq: 1,
	senderRole: "builder",
	targetRole: "reviewer",
	message: "please review",
	mode: "queue",
	commit: "0123456789abcdef",
	batonHolder: "builder",
	state: "queued",
	stateReason: "",
	attempts: 0,
	createdAt: "2026-01-01T00:00:00Z",
	editable: true,
	...over,
});

function wrap(ui: React.ReactNode) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

describe("crew thread", () => {
	beforeEach(() => {
		for (const m of [getMock, patchMock, deleteMock, postMock]) m.mockReset();
		patchMock.mockResolvedValue({ data: message({}), error: undefined });
		deleteMock.mockResolvedValue({ data: message({}), error: undefined });
		postMock.mockResolvedValue({ data: message({}), error: undefined });
	});

	it("shows every message with its sender, target, commit, baton, and delivery state", async () => {
		getMock.mockResolvedValue({
			data: { messages: [message({}), message({ id: "m-2", state: "delivered", editable: false, message: "done", mode: "steer" })] },
			error: undefined,
		});
		wrap(<CrewThreadPane session={owner} />);
		expect(await screen.findByText("please review")).toBeTruthy();
		expect(screen.getAllByText("builder → reviewer")).toHaveLength(2);
		expect(screen.getAllByText("0123456")).toHaveLength(2);
		expect(screen.getByText("Delivered")).toBeTruthy();
		expect(screen.getByText("Queued")).toBeTruthy();
		expect(screen.getByText("steer")).toBeTruthy();
		// A delivered message can no longer be changed.
		const delivered = screen.getByText("done").closest("li") as HTMLElement;
		expect(within(delivered).queryByRole("button")).toBeNull();
	});

	it("edits a queued message through the daemon", async () => {
		getMock.mockResolvedValue({ data: { messages: [message({})] }, error: undefined });
		wrap(<CrewThreadPane session={owner} />);
		await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
		const box = screen.getByRole("textbox", { name: "Edit message" });
		await userEvent.clear(box);
		await userEvent.type(box, "revised");
		await userEvent.click(screen.getByRole("button", { name: "Save" }));
		await waitFor(() =>
			expect(patchMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/messages/{messageId}", {
				params: { path: { sessionId: "w-1", messageId: "m-1" } },
				body: { message: "revised" },
			}),
		);
	});

	it("drops a queued message and releases a held one", async () => {
		getMock.mockResolvedValue({
			data: { messages: [message({}), message({ id: "m-2", state: "held", stateReason: "rate limit: held for a person" })] },
			error: undefined,
		});
		wrap(<CrewThreadPane session={owner} />);
		expect(await screen.findByText("rate limit: held for a person")).toBeTruthy();
		await userEvent.click(screen.getByRole("button", { name: "Release" }));
		await waitFor(() =>
			expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/messages/{messageId}/release", {
				params: { path: { sessionId: "w-1", messageId: "m-2" } },
			}),
		);
		await userEvent.click(screen.getAllByRole("button", { name: "Drop" })[0]);
		await waitFor(() =>
			expect(deleteMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/crew/messages/{messageId}", {
				params: { path: { sessionId: "w-1", messageId: "m-1" } },
			}),
		);
	});

	it("explains an empty thread", async () => {
		getMock.mockResolvedValue({ data: { messages: [] }, error: undefined });
		wrap(<CrewThreadPane session={owner} />);
		expect(await screen.findByText(/No messages yet/)).toBeTruthy();
	});
});
