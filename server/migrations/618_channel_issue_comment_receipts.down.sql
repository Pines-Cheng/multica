ALTER TABLE channel_issue_comment_pending
    DROP COLUMN state,
    DROP COLUMN comment_id,
    DROP COLUMN comment_content,
    DROP COLUMN dispatch_outcomes,
    DROP COLUMN failure_code,
    DROP COLUMN receipt_message_id,
    DROP COLUMN attempt;
