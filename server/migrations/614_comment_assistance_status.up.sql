ALTER TABLE comment ADD COLUMN assistance_status TEXT NOT NULL DEFAULT 'none'
    CHECK (assistance_status IN ('none', 'open', 'closed'));
