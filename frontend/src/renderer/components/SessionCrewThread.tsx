import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { cn } from "../lib/utils";
import type { WorkspaceSession } from "../types/workspace";
import { TerminalTabFrame } from "./TerminalTabFrame";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Textarea } from "./ui/textarea";

export type CrewMessage = components["schemas"]["CrewMessage"];

/** The crew thread is a pseudo role in the task's tab strip; real role ids never take this value. */
export const CREW_THREAD_ID = "crew-thread";
export const crewThreadTabKey = `role:${CREW_THREAD_ID}`;

export const sessionCrewMessagesQueryKey = (sessionId: string, hostId?: string) =>
	hostId ? (["session-crew-messages", hostId, sessionId] as const) : (["session-crew-messages", sessionId] as const);

/** The task's crew thread: every durable role-to-role message with its delivery state. */
export function useCrewMessages(session: WorkspaceSession | undefined, hostId: string | undefined, enabled: boolean) {
	const sessionId = session?.id ?? "";
	return useQuery({
		queryKey: sessionCrewMessagesQueryKey(sessionId, hostId),
		enabled: enabled && Boolean(session),
		refetchInterval: 3000,
		queryFn: async () => {
			const { data, error } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/crew/messages", {
				params: { path: { sessionId } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			return data.messages;
		},
	});
}

type MessageAction = { kind: "edit"; message: string } | { kind: "drop" } | { kind: "release" };

function useCrewMessageAction(sessionId: string, hostId?: string) {
	const queryClient = useQueryClient();
	return useMutation({
		mutationFn: async ({ messageId, action }: { messageId: string; action: MessageAction }) => {
			const client = clientForSessionHost(hostId);
			const params = { path: { sessionId, messageId } };
			const result =
				action.kind === "edit"
					? await client.PATCH("/api/v1/sessions/{sessionId}/crew/messages/{messageId}", { params, body: { message: action.message } })
					: action.kind === "drop"
						? await client.DELETE("/api/v1/sessions/{sessionId}/crew/messages/{messageId}", { params })
						: await client.POST("/api/v1/sessions/{sessionId}/crew/messages/{messageId}/release", { params });
			if (result.error) throw new Error(apiErrorMessage(result.error));
		},
		onSettled: () => queryClient.invalidateQueries({ queryKey: sessionCrewMessagesQueryKey(sessionId, hostId) }),
	});
}

export function CrewThreadTab({ active, onActivate }: { active: boolean; onActivate: () => void }) {
	const { t } = useTranslation();
	return (
		<TerminalTabFrame
			active={active}
			buttonProps={{
				"aria-label": t("session.crew.thread"),
				"aria-selected": active,
				onClick: onActivate,
				role: "tab",
				tabIndex: active ? 0 : -1,
				title: t("session.crew.threadTitle"),
				type: "button",
			}}
			className="max-w-shell-tab-max"
			contentClassName="font-medium"
		>
			<span className="truncate">{t("session.crew.thread")}</span>
		</TerminalTabFrame>
	);
}

const STATE_VARIANT = { delivered: "success", held: "warning", failed: "error", dropped: "outline" } as const;

function MessageRow({ busy, message, onAction }: { busy: boolean; message: CrewMessage; onAction: (action: MessageAction) => void }) {
	const { t } = useTranslation();
	const [draft, setDraft] = useState<string | null>(null);
	const state = message.state as keyof typeof STATE_VARIANT;
	return (
		<li className="flex flex-col gap-1.5 border-b border-border px-4 py-3" data-state={message.state}>
			<div className="flex flex-wrap items-center gap-2 text-sm">
				<span className="font-medium">{t("session.crew.threadFromTo", { from: message.senderRole, to: message.targetRole })}</span>
				<Badge variant={STATE_VARIANT[state] ?? "neutral"}>{t(`session.crew.state.${message.state}` as "session.crew.state.queued")}</Badge>
				{message.mode === "steer" ? <Badge variant="outline">{t("session.crew.steer")}</Badge> : null}
				{message.commit ? <span className="font-mono text-xs text-passive">{message.commit.slice(0, 7)}</span> : null}
				{message.batonHolder ? <span className="text-xs text-passive">{t("session.crew.baton", { role: message.batonHolder })}</span> : null}
				<time className="ml-auto text-xs text-passive" dateTime={message.createdAt}>
					{new Date(message.createdAt).toLocaleTimeString()}
				</time>
			</div>
			{draft !== null ? (
				<div className="flex flex-col gap-2">
					<Textarea aria-label={t("session.crew.editMessage")} onChange={(event) => setDraft(event.target.value)} value={draft} />
					<div className="flex gap-2">
						<Button
							disabled={busy || draft.trim() === ""}
							onClick={() => {
								onAction({ kind: "edit", message: draft });
								setDraft(null);
							}}
							size="sm"
							type="button"
						>
							{t("session.crew.saveEdit")}
						</Button>
						<Button onClick={() => setDraft(null)} size="sm" type="button" variant="ghost">
							{t("session.crew.cancelEdit")}
						</Button>
					</div>
				</div>
			) : (
				<p className={cn("whitespace-pre-wrap break-words text-sm", message.state === "dropped" && "text-passive line-through")}>{message.message}</p>
			)}
			{message.stateReason && message.state !== "delivered" ? <p className="text-xs text-passive">{message.stateReason}</p> : null}
			{message.editable && draft === null ? (
				<div className="flex gap-2">
					{message.state === "held" ? (
						<Button disabled={busy} onClick={() => onAction({ kind: "release" })} size="sm" type="button" variant="outline">
							{t("session.crew.release")}
						</Button>
					) : null}
					<Button disabled={busy} onClick={() => setDraft(message.message)} size="sm" type="button" variant="outline">
						{t("session.crew.edit")}
					</Button>
					<Button disabled={busy} onClick={() => onAction({ kind: "drop" })} size="sm" type="button" variant="ghost">
						{t("session.crew.drop")}
					</Button>
				</div>
			) : null}
		</li>
	);
}

/**
 * Every message the task's roles sent each other, oldest first. A person can
 * edit or drop one that has not been delivered, and release one held back by
 * the rate limit.
 */
export function CrewThreadPane({ hostId, session }: { hostId?: string; session: WorkspaceSession }) {
	const { t } = useTranslation();
	const messages = useCrewMessages(session, hostId, true);
	const act = useCrewMessageAction(session.id, hostId);
	return (
		<section aria-label={t("session.crew.thread")} className="flex h-full flex-col overflow-y-auto bg-background">
			{act.error ? (
				<p className="px-4 py-2 text-sm text-destructive" role="alert">
					{apiErrorMessage(act.error)}
				</p>
			) : null}
			{messages.error ? (
				<p className="px-4 py-2 text-sm text-destructive" role="alert">
					{apiErrorMessage(messages.error)}
				</p>
			) : null}
			{messages.data && messages.data.length === 0 ? <p className="px-4 py-6 text-sm text-passive">{t("session.crew.threadEmpty")}</p> : null}
			<ol>
				{(messages.data ?? []).map((message) => (
					<MessageRow busy={act.isPending} key={message.id} message={message} onAction={(action) => act.mutate({ messageId: message.id, action })} />
				))}
			</ol>
		</section>
	);
}
