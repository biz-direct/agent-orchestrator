import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TooltipProvider } from "./ui/tooltip";

const { getMock, putMock } = vi.hoisted(() => ({ getMock: vi.fn(), putMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PUT: putMock },
	apiErrorMessage: (error: unknown) =>
		typeof error === "object" && error !== null && "message" in error ? String((error as { message: unknown }).message) : "Request failed",
}));

import { ProjectPipelineSettings, selectionValue } from "./ProjectPipelineSettings";

const unavailableReason = "Workflow execution is not available in this build.";

function catalog(overrides: Record<string, unknown> = {}) {
	return {
		projectId: "proj-1",
		profiles: [],
		workflows: [
			{ id: "build-test-review", file: ".ao/pipelines/workflows/build-test-review.yaml", valid: true, diagnostics: [], executable: false, unavailableReason },
			{
				id: "broken",
				file: ".ao/pipelines/workflows/broken.yaml",
				valid: false,
				executable: false,
				diagnostics: [{ file: ".ao/pipelines/workflows/broken.yaml", field: "stages[1].profile", message: 'references unknown profile "ghost"' }],
			},
		],
		diagnostics: [],
		default: { selection: null, state: "unset", executable: false, message: "No default pipeline is set." },
		...overrides,
	};
}

function renderSettings() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<ProjectPipelineSettings projectId="proj-1" />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

describe("ProjectPipelineSettings", () => {
	beforeEach(() => {
		getMock.mockReset();
		putMock.mockReset();
	});

	it("maps stored selections to option values", () => {
		expect(selectionValue(undefined)).toBe("unset");
		expect(selectionValue({ selection: { mode: "normal_worker" } } as never)).toBe("normal");
		expect(selectionValue({ selection: { mode: "workflow", workflowId: "x" } } as never)).toBe("workflow:x");
	});

	it("saves a workflow selection through the dedicated endpoint and shows it as unavailable", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		putMock.mockResolvedValue({
			data: {
				selection: { mode: "workflow", workflowId: "build-test-review" },
				state: "workflow_unavailable",
				executable: false,
				message: unavailableReason,
			},
		});
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: "Default pipeline" }));
		expect(screen.getByRole("menuitem", { name: /broken \(invalid\)/ })).toHaveAttribute("aria-disabled", "true");
		await userEvent.click(screen.getByRole("menuitem", { name: /build-test-review \(unavailable\)/ }));

		await waitFor(() =>
			expect(putMock).toHaveBeenCalledWith("/api/v1/projects/{id}/pipelines/default", {
				params: { path: { id: "proj-1" } },
				body: { selection: { mode: "workflow", workflowId: "build-test-review" } },
			}),
		);
		expect(await screen.findByRole("status")).toHaveTextContent(unavailableReason);
	});

	it("keeps an explicit normal-worker choice distinct from not set", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		putMock.mockResolvedValue({ data: { selection: { mode: "normal_worker" }, state: "normal_worker", executable: false, message: "x" } });
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: "Default pipeline" }));
		await userEvent.click(screen.getByRole("menuitem", { name: "Normal worker" }));
		await waitFor(() =>
			expect(putMock).toHaveBeenCalledWith(
				"/api/v1/projects/{id}/pipelines/default",
				expect.objectContaining({ body: { selection: { mode: "normal_worker" } } }),
			),
		);
	});

	it("lists definition problems with file and field", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: /1 definition problem/ }));
		expect(screen.getByText('references unknown profile "ghost"')).toBeInTheDocument();
		expect(screen.getByText(/broken\.yaml · stages\[1\]\.profile/)).toBeInTheDocument();
	});

	it("flags a selected workflow that vanished from the repository", async () => {
		getMock.mockResolvedValue({
			data: catalog({
				workflows: [],
				default: {
					selection: { mode: "workflow", workflowId: "gone" },
					state: "workflow_missing",
					executable: false,
					message: 'Workflow "gone" is selected but no longer defined.',
				},
			}),
		});
		renderSettings();
		expect(await screen.findByRole("alert")).toHaveTextContent("no longer defined");
	});

	it("surfaces save failures near the control", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		putMock.mockResolvedValue({ error: { message: "Workflow has validation errors" } });
		renderSettings();
		await userEvent.click(await screen.findByRole("button", { name: "Default pipeline" }));
		await userEvent.click(screen.getByRole("menuitem", { name: /build-test-review/ }));
		expect(await screen.findByRole("alert")).toHaveTextContent("Workflow has validation errors");
	});
});
