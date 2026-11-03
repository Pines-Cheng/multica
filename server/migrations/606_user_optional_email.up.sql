-- Feishu-only accounts have no verified email. Keep uniqueness for actual
-- addresses through migration 605, without inventing deliverable addresses.
ALTER TABLE "user" DROP CONSTRAINT IF EXISTS user_email_key;
DROP INDEX IF EXISTS user_email_key;
