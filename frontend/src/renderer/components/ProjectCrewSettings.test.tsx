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

import { ProjectCrewSettings, crewSelectionValue } from "./ProjectCrewSettings";

function catalog(overrides: Record<string, unknown> = {}) {
	return {
		projectId: "proj-1",
		roles: [],
		workflows: [
			{ id: "full", file: ".ao/crew/workflows/full.yaml", valid: true, diagnostics: [] },
			{
				id: "broken",
				file: ".ao/crew/workflows/broken.yaml",
				valid: false,
				diagnostics: [{ file: ".ao/crew/workflows/broken.yaml", field: "roles[1]", message: 'references unknown role "ghost"' }],
			},
		],
		diagnostics: [],
		default: { selection: null, state: "unset", message: "No default crew workflow is set." },
		...overrides,
	};
}

function renderSettings() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<ProjectCrewSettings projectId="proj-1" />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

describe("ProjectCrewSettings", () => {
	beforeEach(() => {
		getMock.mockReset();
		putMock.mockReset();
	});

	it("maps stored selections to option values", () => {
		expect(crewSelectionValue(undefined)).toBe("unset");
		expect(crewSelectionValue({ selection: { mode: "no_crew" } } as never)).toBe("no-crew");
		expect(crewSelectionValue({ selection: { mode: "workflow", workflowId: "x" } } as never)).toBe("workflow:x");
	});

	it("saves a workflow selection through the dedicated endpoint and disables invalid workflows", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		putMock.mockResolvedValue({ data: { selection: { mode: "workflow", workflowId: "full" }, state: "workflow_available", message: "ok" } });
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: "Default crew workflow" }));
		expect(screen.getByRole("menuitem", { name: /broken \(invalid\)/ })).toHaveAttribute("aria-disabled", "true");
		await userEvent.click(screen.getByRole("menuitem", { name: "full" }));

		await waitFor(() =>
			expect(putMock).toHaveBeenCalledWith("/api/v1/projects/{id}/crew/default", {
				params: { path: { id: "proj-1" } },
				body: { selection: { mode: "workflow", workflowId: "full" } },
			}),
		);
	});

	it("keeps an explicit no-crew choice distinct from not set, and can clear", async () => {
		getMock.mockResolvedValue({
			data: catalog({ default: { selection: { mode: "no_crew" }, state: "no_crew", message: "x" } }),
		});
		putMock.mockResolvedValue({ data: { selection: null, state: "unset", message: "x" } });
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: "Default crew workflow" }));
		await userEvent.click(screen.getByRole("menuitem", { name: "Not set" }));
		await waitFor(() =>
			expect(putMock).toHaveBeenCalledWith(
				"/api/v1/projects/{id}/crew/default",
				expect.objectContaining({ body: { selection: null } }),
			),
		);
	});

	it("lists definition problems with file and field", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		renderSettings();

		await userEvent.click(await screen.findByRole("button", { name: /1 definition problem/ }));
		expect(screen.getByText('references unknown role "ghost"')).toBeInTheDocument();
		expect(screen.getByText(/broken\.yaml · roles\[1\]/)).toBeInTheDocument();
	});

	it("flags a selected workflow that vanished from the repository", async () => {
		getMock.mockResolvedValue({
			data: catalog({
				workflows: [],
				default: { selection: { mode: "workflow", workflowId: "gone" }, state: "workflow_missing", message: 'Workflow "gone" is selected but no longer defined.' },
			}),
		});
		renderSettings();
		expect(await screen.findByRole("alert")).toHaveTextContent("no longer defined");
	});

	it("surfaces save failures near the control", async () => {
		getMock.mockResolvedValue({ data: catalog() });
		putMock.mockResolvedValue({ error: { message: "Workflow has validation errors" } });
		renderSettings();
		await userEvent.click(await screen.findByRole("button", { name: "Default crew workflow" }));
		await userEvent.click(screen.getByRole("menuitem", { name: "full" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("Workflow has validation errors");
	});
});
