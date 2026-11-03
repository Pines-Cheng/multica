CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_user_nonempty_email ON "user" (email) WHERE email <> '';
