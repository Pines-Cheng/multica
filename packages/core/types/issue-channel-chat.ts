/** An IM group chat bound to an issue (docs/lark-issue-group-sync.md). */
export interface IssueChannelChatEntry {
  channel_type: string;
  chat_id: string;
  /** Deep link that opens the chat in the IM client. */
  app_link: string;
  created_at: string;
}

export interface IssueChannelChat {
  /** False when the deployment cannot manage IM chats at all. */
  available: boolean;
  /** Whether opening a chat would find a host bot for this issue. */
  can_create: boolean;
  chat: IssueChannelChatEntry | null;
}
