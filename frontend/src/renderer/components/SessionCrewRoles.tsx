import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { UserPlus, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import type { WorkspaceSession } from "../types/workspace";
import { SessionChatSurface } from "./chat/SessionChatSurface";
import { TerminalTabFrame } from "./TerminalTabFrame";
import { TopbarButton } from "./TopbarButton";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuTrigger } from "./ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "./ui/tooltip";

export type CrewRoleSession = components["schemas"]["CrewRoleSession"];

export const roleTabKey = (sessionId: string) => `role:${sessionId}`;

export const sessionCrewRolesQueryKey = (sessionId: string, hostId?: string) =>
	hostId ? (["session-crew-roles", hostId, sessionId] as const) : (["session-crew-roles", sessionId] as const);

/**
 * Role sessions attached to a worker task. A role is a separate Chat
 * conversation in the task's worktree; it is shown as a tab inside the task,
 * never as a board card. Ineligible sessions (orchestrators, cloud, ended
 * tasks) never query.
 */
export function useSessionCrewRoles(session: WorkspaceSession | undefined, hostId?: string) {
	const eligible = Boolean(session) && session?.kind !== "orchestrator" && !session?.cloud && session?.isTerminated !== true;
	const sessionId = session?.id ?? "";
	const query = useQuery({
		queryKey: sessionCrewRolesQueryKey(sessionId, hostId),
		enabled: eligible,
		refetchInterval: 5000,
		queryFn: async () => {
			const { data, error } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/crew/roles", {
				params: { path: { sessionId } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data.roles;
		},
	});
	return { eligible, roles: eligible ? (query.data ?? []) : [] };
}

export function useRemoveCrewRole(sessionId: string, hostId?: string) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async (role: string) => {
			const { error } = await clientForSessionHost(hostId).DELETE("/api/v1/sessions/{sessionId}/crew/roles/{role}", {
				params: { path: { sessionId, role } },
			});
			if (error) throw new Error(apiErrorMessage(error));
		},
		onSettled: () => queryClient.invalidateQueries({ queryKey: sessionCrewRolesQueryKey(sessionId, hostId) }),
	});
}

/** A role's tab: selecting it shows the role's conversation; the cross ends only that conversation. */
export function SessionRoleTab({
	active,
	onActivate,
	onClose,
	role,
}: {
	active: boolean;
	onActivate: () => void;
	onClose: () => void;
	role: CrewRoleSession;
}) {
	const { t } = useTranslation();
	const closeAction = (
		<Tooltip>
			<TooltipTrigger asChild>
				<button
					aria-label={t("session.crew.removeRole", { role: role.role })}
					className="grid size-icon-sm place-items-center rounded-sm text-passive hover:bg-interactive-hover hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-accent/50"
					onClick={(event) => {
						event.stopPropagation();
						onClose();
					}}
					type="button"
				>
					<X aria-hidden="true" className="size-icon-sm" />
				</button>
			</TooltipTrigger>
			<TooltipContent side="bottom">{t("session.crew.removeRole", { role: role.role })}</TooltipContent>
		</Tooltip>
	);
	return (
		<TerminalTabFrame
			action={closeAction}
			active={active}
			buttonProps={{
				"aria-label": role.role,
				"aria-selected": active,
				onClick: onActivate,
				role: "tab",
				tabIndex: active ? 0 : -1,
				title: t("session.crew.roleTabTitle", { role: role.role, harness: role.harness }),
				type: "button",
			}}
			className="max-w-shell-tab-max"
			contentClassName="font-medium"
		>
			<span className="truncate">{role.role}</span>
		</TerminalTabFrame>
	);
}

/** Starts a repository-defined role as a new tab in the task. */
export function AddCrewRoleMenu({
	hostId,
	onAdded,
	roles,
	session,
}: {
	hostId?: string;
	onAdded: (role: CrewRoleSession) => void;
	roles: CrewRoleSession[];
	session: WorkspaceSession;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const client = clientForSessionHost(hostId);
	const catalog = useQuery({
		queryKey: hostId ? ["project-crew", hostId, session.workspaceId] : ["project-crew", session.workspaceId],
		queryFn: async () => {
			const { data, error } = await client.GET("/api/v1/projects/{id}/crew", { params: { path: { id: session.workspaceId } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
	const add = useMutation({
		mutationFn: async (role: string) => {
			const { data, error } = await client.POST("/api/v1/sessions/{sessionId}/crew/roles", {
				params: { path: { sessionId: session.id } },
				body: { role },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
		onSuccess: (role) => {
			void queryClient.invalidateQueries({ queryKey: sessionCrewRolesQueryKey(session.id, hostId) });
			onAdded(role);
		},
	});
	const taken = new Set(roles.map((r) => r.role));
	const available = (catalog.data?.roles ?? []).filter((r) => r.valid && !taken.has(r.id));
	const label = t("session.crew.addRole");
	return (
		<DropdownMenu>
			<Tooltip>
				<TooltipTrigger asChild>
					<DropdownMenuTrigger asChild>
						<TopbarButton aria-label={label} type="button" variant="icon">
							<UserPlus aria-hidden="true" className="size-icon-md" />
						</TopbarButton>
					</DropdownMenuTrigger>
				</TooltipTrigger>
				<TooltipContent side="bottom">{add.error ? apiErrorMessage(add.error) : label}</TooltipContent>
			</Tooltip>
			<DropdownMenuContent align="end">
				<DropdownMenuLabel>{label}</DropdownMenuLabel>
				{available.length === 0 ? (
					<DropdownMenuItem disabled>{t("session.crew.noRoles")}</DropdownMenuItem>
				) : (
					available.map((r) => (
						<DropdownMenuItem disabled={add.isPending} key={r.id} onSelect={() => add.mutate(r.id)}>
							{r.id}
						</DropdownMenuItem>
					))
				)}
				{add.error ? (
					<DropdownMenuItem disabled role="alert">
						{apiErrorMessage(add.error)}
					</DropdownMenuItem>
				) : null}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}

/**
 * A role's Chat conversation. It is the ordinary Chat surface for the role's
 * own session id, so messaging, steering, permission approval, and interrupt
 * work exactly as they do for any Chat session.
 */
export function CrewRoleChat({ hostId, owner, role }: { hostId?: string; owner: WorkspaceSession; role: CrewRoleSession }) {
	const session = {
		id: role.sessionId,
		workspaceId: owner.workspaceId,
		workspaceName: owner.workspaceName,
		title: role.role,
		provider: (role.harness || owner.provider) as WorkspaceSession["provider"],
		kind: "worker",
		mode: "chat",
		status: "working",
	} as WorkspaceSession;
	return <SessionChatSurface hideHeader hostId={hostId} key={role.sessionId} session={session} daemonReady />;
}
