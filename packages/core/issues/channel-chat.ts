import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { IssueChannelChat } from "../types";

export function issueChannelChatOptions(workspaceId: string, issueId: string) {
  return queryOptions({
    queryKey: ["issue-channel-chat", workspaceId, issueId],
    queryFn: () => api.getIssueChannelChat(issueId),
    enabled: !!workspaceId && !!issueId,
    staleTime: 60_000,
  });
}

/** Opens (or returns the existing) IM group chat for an issue. */
export function useCreateIssueChannelChat(workspaceId: string, issueId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => api.createIssueChannelChat(issueId),
    onSuccess: (chat: IssueChannelChat) => {
      client.setQueryData(issueChannelChatOptions(workspaceId, issueId).queryKey, chat);
    },
  });
}
