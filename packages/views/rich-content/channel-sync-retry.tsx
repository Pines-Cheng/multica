"use client";

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { issueKeys } from "@multica/core/issues/queries";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../i18n";

export function channelSyncMessageId(href: string | undefined): string | undefined {
  return href?.match(/^\/api\/channel-messages\/(om_[a-zA-Z0-9_-]+)\/retry$/)?.[1];
}

export function ForwardedSummary() {
  const { t } = useT("editor");
  return <summary className="cursor-pointer font-medium">{t(($) => $.channel_sync.forwarded)}</summary>;
}

export function ChannelSyncRetry({ messageId }: { messageId: string }) {
  const { t } = useT("editor");
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [failed, setFailed] = useState(false);
  async function retry() {
    if (pending) return;
    setPending(true);
    setFailed(false);
    try {
      await api.retryChannelMessageSync(messageId);
      await queryClient.invalidateQueries({ queryKey: issueKeys.timelineAll() });
    } catch {
      setFailed(true);
    } finally {
      setPending(false);
    }
  }
  return (
    <span>
      <Button type="button" variant="link" size="sm" disabled={pending} aria-busy={pending} onClick={() => void retry()}>
        {pending ? t(($) => $.channel_sync.syncing) : t(($) => $.channel_sync.retry)}
      </Button>
      {failed && <span role="alert" className="text-destructive text-caption">{t(($) => $.channel_sync.failed)}</span>}
    </span>
  );
}
