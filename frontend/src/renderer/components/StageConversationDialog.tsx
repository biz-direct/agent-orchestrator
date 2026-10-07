import { useTranslation } from "react-i18next";
import type { WorkspaceSession } from "../types/workspace";
import { SessionChatSurface } from "./chat/SessionChatSurface";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./ui/dialog";

/**
 * The conversation of one attached pipeline stage. A specialist runs as its own
 * conversation inside the task's worktree; it is not a board task, so it opens
 * from the task's Pipeline section instead of from a session list.
 */
export function StageConversationDialog({
	conversationSessionId,
	stageId,
	owner,
	hostId,
	provider,
	onClose,
}: {
	conversationSessionId: string;
	stageId: string;
	owner: WorkspaceSession;
	hostId?: string;
	provider?: string;
	onClose: () => void;
}) {
	const { t } = useTranslation();
	const session = {
		id: conversationSessionId,
		workspaceId: owner.workspaceId,
		workspaceName: owner.workspaceName,
		title: t("inspector.pipeline.conversationTitle", { stage: stageId }),
		provider: (provider ?? owner.provider) as WorkspaceSession["provider"],
		kind: "worker",
		mode: "chat",
		status: "working",
	} as WorkspaceSession;
	return (
		<Dialog open onOpenChange={(open) => !open && onClose()}>
			<DialogContent className="flex h-[80vh] max-w-4xl flex-col gap-2 p-0">
				<div className="px-4 pt-4">
					<DialogTitle className="text-base font-medium">{session.title}</DialogTitle>
					<DialogDescription className="sr-only">{t("inspector.pipeline.conversationDescription", { stage: stageId })}</DialogDescription>
				</div>
				<div className="min-h-0 flex-1">
					<SessionChatSurface hostId={hostId} session={session} daemonReady />
				</div>
			</DialogContent>
		</Dialog>
	);
}
