ALTER TABLE channel_issue_comment_pending
    ADD COLUMN state TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN comment_id UUID,
    ADD COLUMN comment_content TEXT NOT NULL DEFAULT '',
    ADD COLUMN dispatch_outcomes JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN failure_code TEXT NOT NULL DEFAULT '',
    ADD COLUMN receipt_message_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;
