import { apiClient, apiErrorMessage } from "../lib/api-client";
import { clientForHost } from "../lib/host-clients";

export const orchestratorRulesFileQueryKey = (projectId: string, hostId: string | undefined, path: string) =>
	["orchestrator-rules-file", hostId ?? "local", projectId, path] as const;

/**
 * Reads a project's orchestrator rules file through the daemon that owns the
 * project (so it works for remote hosts). Throws with the daemon's message when
 * the path is outside the repo, missing, unreadable, or too large.
 */
export async function fetchOrchestratorRulesFile(projectId: string, hostId: string | undefined, path: string): Promise<string> {
	const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).GET("/api/v1/projects/{id}/orchestrator-rules-file", {
		params: { path: { id: projectId }, query: { path } },
	});
	if (error) throw new Error(apiErrorMessage(error));
	return data?.content ?? "";
}
