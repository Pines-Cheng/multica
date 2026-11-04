"use client";

import { useSetCommentAssistance } from "@multica/core/issues";
import type { TimelineEntry } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { toast } from "sonner";
import { useT } from "../../i18n";

export function CommentAssistance({ issueId, entry, canAct }: { issueId: string; entry: TimelineEntry; canAct: boolean }) {
  if (entry.deleted_at || (entry.assistance_status !== "open" && entry.assistance_status !== "closed")) return null;
  return <AssistanceControls issueId={issueId} entry={entry} canAct={canAct} />;
}

function AssistanceControls({ issueId, entry, canAct }: { issueId: string; entry: TimelineEntry; canAct: boolean }) {
  const { t } = useT("issues");
  const mutation = useSetCommentAssistance(issueId);
  const status = entry.assistance_status;
  return (
    <div className="mb-2 flex flex-wrap items-center gap-2 text-caption">
      <span className={status === "open" ? "text-foreground font-medium" : "text-muted-foreground"}>
        {status === "open" ? t(($) => $.comment.assistance.open) : t(($) => $.comment.assistance.closed)}
      </span>
      {canAct && (
        <Button type="button" variant="ghost" size="xs" disabled={mutation.isPending} aria-busy={mutation.isPending || undefined}
          onClick={() => mutation.mutateAsync({ commentId: entry.id, status: status === "open" ? "closed" : "open" })
            .catch(() => { toast.error(t(($) => $.comment.assistance.update_failed)); })}>
          {status === "open" ? t(($) => $.comment.assistance.close) : t(($) => $.comment.assistance.reopen)}
        </Button>
      )}
    </div>
  );
}
