CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_chat_route_idx ON channel_issue_chat (channel_type, chat_id, thread_root_message_id);
