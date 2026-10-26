-- Durable Markdown continuation cursors. A cursor is bound to the exact
-- principal, file, source version, immutable artifact generation and byte
-- offset. The token is an opaque 256-bit secret; only its digest is stored.
-- format_version lets a later contract change reject old cursors explicitly
-- instead of reinterpreting their offsets.
CREATE TABLE IF NOT EXISTS read_cursors (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    format_version integer NOT NULL DEFAULT 1 CHECK (format_version = 1),
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    file_id uuid NOT NULL,
    source_version_id uuid NOT NULL,
    artifact_set_id uuid NOT NULL,
    read_scope text NOT NULL CHECK (read_scope IN ('current', 'history')),
    byte_offset bigint NOT NULL CHECK (byte_offset >= 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (file_id, source_version_id) REFERENCES file_versions(file_id, id) ON DELETE CASCADE,
    FOREIGN KEY (source_version_id, artifact_set_id) REFERENCES artifact_sets(source_version_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS read_cursors_expiry ON read_cursors(expires_at);
CREATE INDEX IF NOT EXISTS read_cursors_principal_file ON read_cursors(principal_id, file_id);
