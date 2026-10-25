/**
 * Daemon-owned user preferences.
 *
 * Read from and written to the daemon rather than held in the renderer, because
 * `ao spawn`, mobile, and headless spawns resolve the same value. A preference
 * kept in one client would look correct in Settings and disagree with the others.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { clientForHost } from "../lib/host-clients";
import type { SessionMode } from "../types/workspace";

/** `disabledModels` lists the models pickers leave out; empty or unset means every catalog model. */
export type HarnessDefault = Pick<components["schemas"]["HarnessDefault"], "disabledModels">;

export const settingsQueryKey = ["settings"] as const;

export interface Settings {
	harnessDefaults?: Record<string, HarnessDefault>;
	/** Applies to sessions created from now on; never to an existing one. */
	defaultSessionMode: SessionMode;
	/** Agents that can run in chat mode today. Empty means chat is unavailable. */
	chatHarnesses: string[];
	/** Deployment client identity (AO_CLIENT); empty when unset. */
	client: string;
	/** Whether the local offering is available on this daemon. */
	localEnabled: boolean;
	/** The user's persisted cloud toggle (Settings, Developer Mode). */
	cloudOffering: boolean;
	/** Whether the cloud offering is available (flag + entitled client + control plane). */
	cloudEnabled: boolean;
	/** Cloud control plane base URL; empty when cloud is not configured. */
	cloudControlPlaneUrl: string;
	/** Whether the daemon's AO_TRACKER_INTAKE gate is on. Off hides intake controls. */
	trackerIntakeEnabled: boolean;
<<<<<<< HEAD
	/** This daemon's global orchestrator prompt; empty means none. */
	globalOrchestratorRules: string;
	/** This daemon's global worker prompt; empty means none. */
	globalWorkerRules: string;
=======
	/** Idle open sessions move to a harness release installed while they ran. */
	versionRefresh?: boolean;
>>>>>>> upstream/main
}

export function useSettings(hostId?: string, enabled = true) {
	const query = useQuery({
		queryKey: hostId ? ["settings", hostId] : settingsQueryKey,
		enabled,
		// Settings gate the cloud sign-in UI, so this query must recover from a
		// transient startup failure. The daemon can still be booting on first
		// fetch ("AO daemon is starting"); without a refetch the whole cloud
		// offering stays hidden until a manual reload. Poll like the workspace
		// query so it self-heals once the daemon is ready.
		refetchInterval: 15_000,
		retry: 5,
		queryFn: async (): Promise<Settings> => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).GET("/api/v1/settings");
			if (error) throw error;
			return {
				defaultSessionMode: (data?.defaultSessionMode ?? "tui") as SessionMode,
				harnessDefaults: data?.harnessDefaults ?? {},
				chatHarnesses: data?.chatHarnesses ?? [],
				client: data?.client ?? "",
				// Offering gates fail closed for cloud and open for local, so a daemon
				// that predates them behaves like a plain local install.
				localEnabled: data?.localEnabled ?? true,
				cloudOffering: data?.cloudOffering ?? false,
				cloudEnabled: data?.cloudEnabled ?? false,
				cloudControlPlaneUrl: data?.cloudControlPlaneUrl ?? "",
				trackerIntakeEnabled: data?.trackerIntakeEnabled ?? false,
<<<<<<< HEAD
				globalOrchestratorRules: data?.globalOrchestratorRules ?? "",
				globalWorkerRules: data?.globalWorkerRules ?? "",
=======
				// A daemon that predates the setting never moves sessions.
				versionRefresh: data?.versionRefresh ?? false,
>>>>>>> upstream/main
			};
		},
	});

	return {
		settings: query.data,
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
	};
}

export function useUpdateSessionInterface() {
	const queryClient = useQueryClient();
	const mutation = useMutation({
		mutationFn: async (mode: SessionMode) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/session-interface", {
				body: { defaultSessionMode: mode },
			});
			if (error) throw error;
			return data;
		},
		// Refetch rather than writing the value in locally: the daemon is the source
		// of truth, and the control must not claim a change it did not persist.
		onSuccess: () => queryClient.invalidateQueries({ queryKey: settingsQueryKey }),
	});

	return {
		update: (mode: SessionMode) => mutation.mutate(mode),
		saving: mutation.isPending,
		error: mutation.error ? apiErrorMessage(mutation.error) : undefined,
	};
}

export function useUpdateCloudOffering() {
	const queryClient = useQueryClient();
	const mutation = useMutation({
		mutationFn: async (enabled: boolean) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/cloud-offering", {
				body: { enabled },
			});
			if (error) throw error;
			return data;
		},
		// Refetch rather than writing the value in locally: the daemon is the source
		// of truth, and the control must not claim a change it did not persist.
		onSuccess: () => queryClient.invalidateQueries({ queryKey: settingsQueryKey }),
	});

	return {
		update: (enabled: boolean) => mutation.mutate(enabled),
		saving: mutation.isPending,
		error: mutation.error ? apiErrorMessage(mutation.error) : undefined,
	};
}

<<<<<<< HEAD
/** Saves this computer's global orchestrator prompt (the local daemon only). */
export function useUpdateGlobalOrchestratorRules() {
	const queryClient = useQueryClient();
	const mutation = useMutation({
		mutationFn: async (rules: string) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/global-orchestrator-rules", {
				body: { globalOrchestratorRules: rules },
=======
/** Saves which models one harness offers without replacing other harnesses' entries. */
export function useUpdateHarnessDefault(hostId?: string) {
	const queryClient = useQueryClient();
	const key = hostId ? ["settings", hostId] : settingsQueryKey;
	const mutation = useMutation({
		mutationFn: async ({ agentId, disabledModels }: { agentId: string; disabledModels: string[] }) => {
			const { data, error } = await (hostId ? clientForHost(hostId) : apiClient).PATCH("/api/v1/settings/harness-defaults/{agent}", {
				params: { path: { agent: agentId } }, body: { disabledModels },
>>>>>>> upstream/main
			});
			if (error) throw error;
			return data;
		},
<<<<<<< HEAD
		onSuccess: (_data, rules) => {
			// Keep the acknowledged text even if the read-back below fails.
			queryClient.setQueryData<Settings>(settingsQueryKey, (current) => current && { ...current, globalOrchestratorRules: rules });
			void queryClient.invalidateQueries({ queryKey: settingsQueryKey });
		},
	});

	return {
		update: (rules: string) => mutation.mutateAsync(rules),
		saving: mutation.isPending,
		error: mutation.error ? apiErrorMessage(mutation.error) : undefined,
	};
}

/** Saves this computer's global worker prompt (the local daemon only). */
export function useUpdateGlobalWorkerRules() {
	const queryClient = useQueryClient();
	const mutation = useMutation({
		mutationFn: async (rules: string) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/global-worker-rules", {
				body: { globalWorkerRules: rules },
=======
		onSuccess: async (data) => {
			queryClient.setQueryData<Settings>(key, (current) => current ? { ...current, harnessDefaults: data?.harnessDefaults ?? {} } : current);
			await queryClient.invalidateQueries({ queryKey: key, exact: true });
		},
	});
	return { update: mutation.mutateAsync, error: mutation.error ? apiErrorMessage(mutation.error) : undefined };
}

export function useUpdateVersionRefresh() {
	const queryClient = useQueryClient();
	const mutation = useMutation({
		mutationFn: async (enabled: boolean) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/version-refresh", {
				body: { enabled },
>>>>>>> upstream/main
			});
			if (error) throw error;
			return data;
		},
<<<<<<< HEAD
		onSuccess: (_data, rules) => {
			// Keep the acknowledged text even if the read-back below fails.
			queryClient.setQueryData<Settings>(settingsQueryKey, (current) => current && { ...current, globalWorkerRules: rules });
			void queryClient.invalidateQueries({ queryKey: settingsQueryKey });
		},
	});

	return {
		update: (rules: string) => mutation.mutateAsync(rules),
=======
		// Refetch rather than writing the value in locally: the daemon is the source
		// of truth, and the control must not claim a change it did not persist.
		onSuccess: () => queryClient.invalidateQueries({ queryKey: settingsQueryKey }),
	});

	return {
		update: (enabled: boolean) => mutation.mutate(enabled),
>>>>>>> upstream/main
		saving: mutation.isPending,
		error: mutation.error ? apiErrorMessage(mutation.error) : undefined,
	};
}
