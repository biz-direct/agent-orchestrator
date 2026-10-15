import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { PromptApplyNotice } from "./PromptApplyNotice";

const { navigateMock, sessionsMock } = vi.hoisted(() => ({ navigateMock: vi.fn(), sessionsMock: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useRouter: () => ({ navigate: navigateMock }) }));
vi.mock("../../hooks/usePromptViews", () => ({ useRolePromptSessions: sessionsMock }));

const closeSettings = vi.hoisted(() => vi.fn());
vi.mock("../../stores/ui-store", () => ({
	useUiStore: (select: (state: { closeSettings: () => void }) => unknown) => select({ closeSettings }),
}));

beforeEach(() => {
	navigateMock.mockReset();
	closeSettings.mockReset();
	sessionsMock.mockReset().mockReturnValue([
		{
			isLoading: false,
			isError: false,
			data: [
				{ sessionId: "app-1", projectId: "app", role: "worker", freshness: "stale" },
				{ sessionId: "solo-1", projectId: "", role: "worker", freshness: "unknown" },
				{ sessionId: "app-2", projectId: "app", role: "worker", freshness: "current" },
			],
		},
	]);
});

it("opens a local project session, a standalone session, and closes Settings", async () => {
	render(<PromptApplyNotice roles={["worker"]} />);
	expect(screen.queryByRole("button", { name: "Open session app-2" })).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("button", { name: "Open session app-1" }));
	expect(navigateMock).toHaveBeenLastCalledWith({ to: "/projects/$projectId/sessions/$sessionId", params: { projectId: "app", sessionId: "app-1" } });
	await userEvent.click(screen.getByRole("button", { name: "Open session solo-1" }));
	expect(navigateMock).toHaveBeenLastCalledWith({ to: "/sessions/$sessionId", params: { sessionId: "solo-1" } });
	expect(closeSettings).toHaveBeenCalledTimes(2);
});

it("keeps the owning host for remote sessions", async () => {
	render(<PromptApplyNotice roles={["worker"]} projectId="app" hostId="host-b" />);
	await userEvent.click(screen.getByRole("button", { name: "Open session app-1" }));
	expect(navigateMock).toHaveBeenLastCalledWith({
		to: "/host/$hostId/project/$projectId/session/$sessionId",
		params: { hostId: "host-b", projectId: "app", sessionId: "app-1" },
	});
	await userEvent.click(screen.getByRole("button", { name: "Open session solo-1" }));
	expect(navigateMock).toHaveBeenLastCalledWith({ to: "/host/$hostId/session/$sessionId", params: { hostId: "host-b", sessionId: "solo-1" } });
});
