import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { PromptsSettingsSection } from "./PromptsSettingsSection";

const { getMock, patchMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn() }));
vi.mock("../../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../../lib/api-client")>()),
	apiClient: { GET: getMock, PATCH: patchMock },
}));

let serverRules = "Saved global.";
let serverWorkerRules = "Saved worker.";
let staleSessions: Array<{ sessionId: string; projectId: string; role: string; freshness: string }> = [];

const orchestratorEditor = () => within(screen.getByTestId("global-rules-orchestrator"));
const workerEditor = () => within(screen.getByTestId("global-rules-worker"));

beforeEach(() => {
	serverRules = "Saved global.";
	serverWorkerRules = "Saved worker.";
	staleSessions = [];
	getMock.mockReset().mockImplementation(async (path: string, init?: { params?: { query?: { role?: string } } }) => {
		if (path === "/api/v1/prompts/sessions") {
			const role = init?.params?.query?.role;
			return { data: { sessions: staleSessions.filter((session) => session.role === role) }, error: undefined };
		}
		if (path === "/api/v1/prompts/builtin") {
			return {
				data: {
					sample: { projectId: "sample-project", projectName: "Sample Project", repo: "https://github.com/example/sample-project", defaultBranch: "main", path: "/p", orchestratorSessionId: "o-1" },
					orchestrator: [{ layer: "builtin", title: "AO Orchestrator Role", text: "## AO Orchestrator Role\nYou coordinate." }],
					worker: [{ layer: "builtin", title: "AO Worker Role", text: "## AO Worker Role\nYou implement." }],
					standaloneWorker: [{ layer: "builtin", title: "AO Standalone Agent", text: "## AO Standalone Agent\nAlone." }],
				},
				error: undefined,
			};
		}
		return { data: { globalOrchestratorRules: serverRules, globalWorkerRules: serverWorkerRules }, error: undefined };
	});
	patchMock.mockReset().mockImplementation(async (path: string, init: { body: { globalOrchestratorRules?: string; globalWorkerRules?: string } }) => {
		if (path.endsWith("global-worker-rules")) serverWorkerRules = init.body.globalWorkerRules ?? "";
		else serverRules = init.body.globalOrchestratorRules ?? "";
		return { data: {}, error: undefined };
	});
});

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	render(<QueryClientProvider client={client}><PromptsSettingsSection /></QueryClientProvider>);
	return client;
}

async function textarea() {
	const field = (await screen.findByLabelText("Global orchestrator prompt")) as HTMLTextAreaElement;
	await waitFor(() => expect(field.value).toBe(serverRules));
	return field;
}

it("keeps a long typed text and saves nothing until Save is clicked", async () => {
	renderSection();
	const field = await textarea();
	const long = "Global rule. ".repeat(80);
	await userEvent.clear(field);
	await userEvent.paste(long);
	await new Promise((resolve) => setTimeout(resolve, 800));
	expect(field.value).toBe(long);
	expect(patchMock).not.toHaveBeenCalled();
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Save" }));
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
	expect(patchMock.mock.calls[0][1].body.globalOrchestratorRules).toBe(long);
	await waitFor(() => expect(orchestratorEditor().getByRole("button", { name: "Save" })).toBeDisabled());
	expect(orchestratorEditor().getAllByRole("status")[0]).toHaveTextContent("Saved");
});

it("discards edits back to the saved value", async () => {
	renderSection();
	const field = await textarea();
	expect(orchestratorEditor().getByRole("button", { name: "Save" })).toBeDisabled();
	expect(orchestratorEditor().getByRole("button", { name: "Discard" })).toBeDisabled();
	await userEvent.type(field, " extra");
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Discard" }));
	expect(field.value).toBe("Saved global.");
	expect(patchMock).not.toHaveBeenCalled();
});

it("does not let a background refetch overwrite unsaved edits", async () => {
	const client = renderSection();
	const field = await textarea();
	await userEvent.type(field, " local");
	serverRules = "Changed elsewhere.";
	await client.invalidateQueries();
	await waitFor(() => expect(getMock.mock.calls.length).toBeGreaterThan(1));
	expect(field.value).toBe("Saved global. local");
});

it("saves with Cmd/Ctrl+S and blocks an over-size prompt inline", async () => {
	renderSection();
	const field = await textarea();
	await userEvent.type(field, " hotkey");
	await userEvent.keyboard("{Control>}s{/Control}");
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));

	await userEvent.click(field);
	await userEvent.clear(field);
	await userEvent.paste("x".repeat(32 * 1024 + 1));
	expect(await screen.findByText("Too large: the limit is 32 KiB.")).toBeInTheDocument();
	expect(orchestratorEditor().getByRole("button", { name: "Save" })).toBeDisabled();
	await userEvent.keyboard("{Control>}s{/Control}");
	expect(patchMock).toHaveBeenCalledTimes(1);
});

it("keeps text typed while a save is pending as an unsaved draft", async () => {
	let finish: () => void = () => undefined;
	patchMock.mockImplementationOnce(async (_path: string, init: { body: { globalOrchestratorRules: string } }) => {
		await new Promise<void>((resolve) => { finish = resolve; });
		serverRules = init.body.globalOrchestratorRules;
		return { data: {}, error: undefined };
	});
	renderSection();
	const field = await textarea();
	await userEvent.type(field, " first");
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Save" }));
	await userEvent.type(field, " newer");
	finish();
	await waitFor(() => expect(getMock.mock.calls.length).toBeGreaterThan(1));
	await waitFor(() => expect(orchestratorEditor().getByRole("button", { name: "Save" })).toBeEnabled());
	expect(field.value).toBe("Saved global. first newer");
});

it("keeps acknowledged text when the post-save refresh fails", async () => {
	renderSection();
	const field = await textarea();
	await userEvent.type(field, " new");
	getMock.mockRejectedValue(new Error("offline"));
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Save" }));
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
	await waitFor(() => expect(orchestratorEditor().getAllByRole("status")[0]).toHaveTextContent("Saved"));
	expect(field.value).toBe("Saved global. new");
});

it("saves the global worker prompt on its own, without touching the orchestrator one", async () => {
	renderSection();
	const field = (await screen.findByLabelText("Global worker prompt")) as HTMLTextAreaElement;
	await waitFor(() => expect(field.value).toBe("Saved worker."));
	expect(workerEditor().getByRole("button", { name: "Save" })).toBeDisabled();
	await userEvent.type(field, " more");
	expect(orchestratorEditor().getByRole("button", { name: "Save" })).toBeDisabled();
	await userEvent.click(workerEditor().getByRole("button", { name: "Save" }));
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
	expect(patchMock.mock.calls[0][0]).toBe("/api/v1/settings/global-worker-rules");
	expect(patchMock.mock.calls[0][1].body).toEqual({ globalWorkerRules: "Saved worker. more" });
	await waitFor(() => expect(workerEditor().getByRole("button", { name: "Save" })).toBeDisabled());
});

it("discards a worker draft and blocks an over-size worker prompt", async () => {
	renderSection();
	const field = (await screen.findByLabelText("Global worker prompt")) as HTMLTextAreaElement;
	await waitFor(() => expect(field.value).toBe("Saved worker."));
	await userEvent.type(field, " draft");
	await userEvent.click(workerEditor().getByRole("button", { name: "Discard" }));
	expect(field.value).toBe("Saved worker.");
	await userEvent.clear(field);
	await userEvent.paste("x".repeat(32 * 1024 + 1));
	expect(await workerEditor().findByText("Too large: the limit is 32 KiB.")).toBeInTheDocument();
	expect(workerEditor().getByRole("button", { name: "Save" })).toBeDisabled();
	expect(patchMock).not.toHaveBeenCalled();
});

it("after saving lists running sessions that still use the previous rules", async () => {
	staleSessions = [
		{ sessionId: "app-orchestrator-1", projectId: "app", role: "orchestrator", freshness: "stale" },
		{ sessionId: "app-2", projectId: "app", role: "worker", freshness: "current" },
	];
	renderSection();
	const field = await textarea();
	await userEvent.type(field, " more");
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Save" }));
	const notice = await orchestratorEditor().findByTestId("prompt-apply-notice");
	expect(await within(notice).findByText("app-orchestrator-1")).toBeInTheDocument();
	expect(within(notice).queryByText("app-2")).not.toBeInTheDocument();
});

it("says so when no running session uses the previous rules", async () => {
	renderSection();
	const field = await textarea();
	await userEvent.type(field, " more");
	await userEvent.click(orchestratorEditor().getByRole("button", { name: "Save" }));
	expect(await orchestratorEditor().findByText("No running sessions use the previous rules.")).toBeInTheDocument();
});

it("shows AO's built-in prompts read-only, labelled as sample, with Copy", async () => {
	const writeText = vi.spyOn(window.ao.clipboard, "writeText");
	renderSection();
	expect(screen.queryByTestId("builtin-orchestrator")).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("button", { name: "Built-in prompts" }));
	expect(await screen.findByTestId("builtin-sample")).toHaveTextContent("Sample Project");
	await userEvent.click(await screen.findByRole("button", { name: "Orchestrator prompt" }));
	const section = await screen.findByTestId("builtin-orchestrator");
	await userEvent.click(within(section).getByRole("button", { name: /AO Orchestrator Role/ }));
	expect(await within(section).findByText(/You coordinate\./)).toBeInTheDocument();
	expect(within(section).getByTestId("prompt-layer")).toHaveTextContent("Built-in");
	expect(screen.queryByRole("textbox", { name: /AO Orchestrator Role/ })).not.toBeInTheDocument();
	await userEvent.click(within(section).getByRole("button", { name: "Copy AO Orchestrator Role" }));
	expect(writeText).toHaveBeenCalledWith("## AO Orchestrator Role\nYou coordinate.");
});
