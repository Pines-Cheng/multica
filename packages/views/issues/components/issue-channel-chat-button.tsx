"use client";

import { memo } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { MessagesSquare } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { issueChannelChatOptions, useCreateIssueChannelChat } from "@multica/core/issues";
import { useWorkspaceId } from "@multica/core/hooks";
import { openExternal } from "../../platform";
import { useT } from "../../i18n";

/**
 * Header action for the issue's Feishu group chat: opens the bound chat, or
 * creates it when the issue's squad leader / assignee agent has a connected
 * bot. Hidden when neither applies.
 */
export const IssueChannelChatButton = memo(function IssueChannelChatButton({
  issueId,
}: {
  issueId: string;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { data } = useQuery(issueChannelChatOptions(wsId, issueId));
  const create = useCreateIssueChannelChat(wsId, issueId);

  if (!data?.available) return null;
  const chat = data.chat;
  if (!chat && !data.can_create) return null;

  const label = chat
    ? t(($) => $.detail.channel_chat_open)
    : create.isPending
      ? t(($) => $.detail.channel_chat_creating)
      : t(($) => $.detail.channel_chat_create);

  function handleClick() {
    if (chat) {
      openExternal(chat.app_link);
      return;
    }
    create.mutate(undefined, {
      onSuccess: (res) => {
        toast.success(t(($) => $.detail.channel_chat_created));
        if (res.chat) openExternal(res.chat.app_link);
      },
      onError: (e) => {
        toast.error(e instanceof Error ? e.message : t(($) => $.detail.channel_chat_create_failed));
      },
    });
  }

  return (
    <TooltipHint label={label}>
      <Button
        variant="ghost"
        size="icon-sm"
        className="text-muted-foreground"
        aria-label={label}
        disabled={create.isPending}
        onClick={handleClick}
        data-testid="issue-channel-chat-button"
      >
        <MessagesSquare />
      </Button>
    </TooltipHint>
  );
});
