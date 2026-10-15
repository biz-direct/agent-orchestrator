import { useQueries, useQuery, type QueryClient } from "@tanstack/react-query";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { clientForHost } from "../lib/host-clients";
import type { components } from "../../api/schema";

export type PromptRole = "orchestrator" | "worker";
export type PromptSection = components["schemas"]["SessionManagerPromptSection"];
export type PromptSession = components["schemas"]["SessionManagerPromptSession"];
export type BuiltinPrompts = components["schemas"]["ControllersBuiltinPromptsResponse"];
export type EffectivePrompt = components["schemas"]["ControllersEffectivePromptResponse"];

const clientFor = (hostId?: string) => (hostId ? clientForHost(hostId) : apiClient);

export const builtinPromptsQueryKey = (hostId?: string) => ["prompts", "builtin", hostId ?? "local"] as const;
export const effectivePromptQueryKey = (hostId: string | undefined, projectId: string, role: PromptRole) =>
	["prompts", "effective", hostId ?? "local", projectId, role] as const;
export const promptSessionsQueryKey = (hostId: string | undefined, projectId: string | undefined, role: PromptRole) =>
	["prompts", "sessions", hostId ?? "local", projectId ?? "all", role] as const;

/** AO's built-in prompts, rendered by the daemon with sample project values. */
export function useBuiltinPrompts(hostId?: string, enabled = true) {
	return useQuery({
		queryKey: builtinPromptsQueryKey(hostId),
		enabled,
		retry: false,
		queryFn: async (): Promise<BuiltinPrompts> => {
			const { data, error } = await clientFor(hostId).GET("/api/v1/prompts/builtin");
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
}

/** The assembled prompt a new session of `role` would get in the project right now. */
export function useEffectivePrompt(projectId: string, role: PromptRole, hostId?: string, enabled = true) {
	return useQuery({
		queryKey: effectivePromptQueryKey(hostId, projectId, role),
		enabled,
		retry: false,
		queryFn: async (): Promise<EffectivePrompt> => {
			const { data, error } = await clientFor(hostId).GET("/api/v1/projects/{id}/effective-prompt", {
				params: { path: { id: projectId }, query: { role } },
			});
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
}

/** Live sessions of `role` (in one project, or all) and whether each runs an older prompt. */
export function usePromptSessions(role: PromptRole, projectId?: string, hostId?: string, enabled = true) {
	return useQuery({
		queryKey: promptSessionsQueryKey(hostId, projectId, role),
		enabled,
		retry: false,
		queryFn: () => fetchPromptSessions(role, projectId, hostId),
	});
}

/** `usePromptSessions` for several roles at once, in the order given. */
export function useRolePromptSessions(roles: PromptRole[], projectId?: string, hostId?: string) {
	return useQueries({
		queries: roles.map((role) => ({
			queryKey: promptSessionsQueryKey(hostId, projectId, role),
			retry: false,
			queryFn: () => fetchPromptSessions(role, projectId, hostId),
		})),
	});
}

async function fetchPromptSessions(role: PromptRole, projectId: string | undefined, hostId: string | undefined): Promise<PromptSession[]> {
	const { data, error } = await clientFor(hostId).GET("/api/v1/prompts/sessions", {
		params: { query: { role, projectId } },
	});
	if (error || !data) throw new Error(apiErrorMessage(error));
	return data.sessions;
}

/** Drops cached prompt views after a rules save so they re-read the daemon. */
export function invalidatePromptViews(queryClient: QueryClient) {
	return queryClient.invalidateQueries({ queryKey: ["prompts"] });
}

export const agentRulesFileQueryKey = (projectId: string, hostId: string | undefined, path: string) =>
	["agent-rules-file", hostId ?? "local", projectId, path] as const;

/** Reads a project's worker rules file through the daemon that owns the project. */
export async function fetchAgentRulesFile(projectId: string, hostId: string | undefined, path: string): Promise<string> {
	const { data, error } = await clientFor(hostId).GET("/api/v1/projects/{id}/agent-rules-file", {
		params: { path: { id: projectId }, query: { path } },
	});
	if (error) throw new Error(apiErrorMessage(error));
	return data?.content ?? "";
}
