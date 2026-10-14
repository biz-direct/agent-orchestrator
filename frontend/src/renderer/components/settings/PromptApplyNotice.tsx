import { useRouter } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useRolePromptSessions, type PromptRole, type PromptSession } from "../../hooks/usePromptViews";
import { sessionNavigateTarget } from "../../lib/navigate-to-session";
import { useUiStore } from "../../stores/ui-store";
import { Button } from "../ui/button";

/** Opens a session and closes Settings; undefined when there is no router (tests, isolated renders). */
function useOpenSession(hostId?: string) {
	const router = useRouter({ warn: false });
	const closeSettings = useUiStore((state) => state.closeSettings);
	if (!router) return undefined;
	return (projectId: string | undefined, sessionId: string) => {
		closeSettings();
		// Keeps the owning host for remote sessions and uses the standalone route when there is no project.
		void router.navigate(sessionNavigateTarget(projectId || undefined, sessionId, hostId));
	};
}

function SessionList({ role, sessions, hostId }: { role: PromptRole; sessions: PromptSession[]; hostId?: string }) {
	const { t } = useTranslation();
	const openSession = useOpenSession(hostId);
	return (
		<div className="flex flex-col gap-1" data-testid={`apply-${role}`}>
			<p className="text-xs font-medium text-foreground">{t(role === "orchestrator" ? "settings.prompts.apply.orchestrators" : "settings.prompts.apply.workers")}</p>
			<ul className="flex flex-wrap gap-2">
				{sessions.map((session) => (
					<li key={session.sessionId} className="flex items-center gap-1">
						{openSession ? (
							<Button
								type="button"
								size="sm"
								variant="secondary"
								aria-label={t("settings.prompts.apply.open", { id: session.sessionId })}
								onClick={() => openSession(session.projectId, session.sessionId)}
							>
								{session.sessionId}
							</Button>
						) : (
							<span className="font-mono text-xs">{session.sessionId}</span>
						)}
						{session.freshness === "unknown" ? <span className="text-xs text-muted-foreground">{t("settings.prompts.apply.unknown")}</span> : null}
					</li>
				))}
			</ul>
		</div>
	);
}

/**
 * Shown after a save: which running sessions still use the previous rules. New
 * rules reach a session only when it is spawned or restored. AO has no safe way
 * to restart a running session (restore only applies to ended ones), so this
 * links to each session rather than offering an action.
 */
export function PromptApplyNotice({ roles, projectId, hostId }: { roles: PromptRole[]; projectId?: string; hostId?: string }) {
	const { t } = useTranslation();
	const lists = useRolePromptSessions(roles, projectId, hostId);
	const loading = lists.some((list) => list.isLoading);
	const failed = lists.some((list) => list.isError);
	const affected = roles.map((role, index) => ({
		role,
		sessions: (lists[index]?.data ?? []).filter((session) => session.freshness !== "current"),
	}));
	const any = affected.some((entry) => entry.sessions.length > 0);
	return (
		<div role="status" className="flex flex-col gap-2 rounded-md bg-input/30 px-3 py-2" data-testid="prompt-apply-notice">
			<p className="text-xs font-medium text-foreground">{t("settings.prompts.apply.title")}</p>
			<p className="text-xs leading-relaxed text-muted-foreground">{t("settings.prompts.apply.help")}</p>
			{failed ? <p className="text-xs text-destructive">{t("settings.prompts.apply.loadFailed")}</p> : null}
			{!loading && !failed && !any ? <p className="text-xs text-muted-foreground">{t("settings.prompts.apply.none")}</p> : null}
			{affected.filter((entry) => entry.sessions.length > 0).map((entry) => (
				<SessionList key={entry.role} role={entry.role} sessions={entry.sessions} hostId={hostId} />
			))}
		</div>
	);
}
