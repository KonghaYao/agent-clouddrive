ALTER TABLE spaces ADD COLUMN native_root_resource_id text NOT NULL DEFAULT '';
-- Runtime-only encrypted delegated credentials are deliberately NOT stored here.
-- Access tickets authorize business resources but never replace authoritative
-- account checks or oCIS service delegation.
