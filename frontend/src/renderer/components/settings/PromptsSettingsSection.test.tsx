import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { PromptsSettingsSection } from "./PromptsSettingsSection";

const { getMock, patchMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn() }));
vi.mock("../../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../../lib/api-client")>()),
	apiClient: { GET: getMock, PATCH: patchMock },
}));

let serverRules = "Saved global.";

beforeEach(() => {
	serverRules = "Saved global.";
	getMock.mockReset().mockImplementation(async () => ({ data: { globalOrchestratorRules: serverRules }, error: undefined }));
	patchMock.mockReset().mockImplementation(async (_path: string, init: { body: { globalOrchestratorRules: string } }) => {
		serverRules = init.body.globalOrchestratorRules;
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
	await userEvent.click(screen.getByRole("button", { name: "Save" }));
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
	expect(patchMock.mock.calls[0][1].body.globalOrchestratorRules).toBe(long);
	await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeDisabled());
	expect(screen.getByRole("status")).toHaveTextContent("Saved");
});

it("discards edits back to the saved value", async () => {
	renderSection();
	const field = await textarea();
	expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
	expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
	await userEvent.type(field, " extra");
	await userEvent.click(screen.getByRole("button", { name: "Discard" }));
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
	expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
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
	await userEvent.click(screen.getByRole("button", { name: "Save" }));
	await userEvent.type(field, " newer");
	finish();
	await waitFor(() => expect(getMock.mock.calls.length).toBeGreaterThan(1));
	await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
	expect(field.value).toBe("Saved global. first newer");
});
