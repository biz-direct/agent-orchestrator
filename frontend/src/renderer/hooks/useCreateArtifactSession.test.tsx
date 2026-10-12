import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../lib/api-client";
import { useCreateArtifactSession } from "./useCreateArtifactSession";

vi.mock("../lib/navigate-to-session", () => ({ useNavigateToSession: () => vi.fn() }));
vi.mock("../lib/telemetry", () => ({ captureRendererEvent: vi.fn() }));
vi.mock("./useAgentReadinessQuery", () => ({ useAgentReadinessQuery: () => ({ data: undefined }) }));

function wrapper(client: QueryClient) {
	return ({ children }: PropsWithChildren) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

describe("useCreateArtifactSession", () => {
	afterEach(() => vi.restoreAllMocks());

	it("opts the artifact worker out of the project's default pipeline", async () => {
		vi.spyOn(apiClient, "POST").mockResolvedValue({ data: { session: { id: "s1" } } } as never);
		vi.spyOn(apiClient, "GET").mockResolvedValue({ data: { status: "ok", project: { agent: "claude-code" } } } as never);
		const client = new QueryClient();
		const { result } = renderHook(() => useCreateArtifactSession(), { wrapper: wrapper(client) });

		await act(async () => {
			await result.current.create("p1");
		});

		expect(apiClient.POST).toHaveBeenCalledWith(
			"/api/v1/sessions",
			expect.objectContaining({
				body: expect.objectContaining({ requestedOutput: "artifact", projectId: "p1", pipeline: { mode: "normal_worker" } }),
			}),
		);
	});
});
