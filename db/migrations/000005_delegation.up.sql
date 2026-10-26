ALTER TABLE upload_sessions ADD COLUMN native_resource_id text;
ALTER TABLE upload_sessions ADD COLUMN transfer_completed boolean NOT NULL DEFAULT false;
CREATE TABLE delegation_grants (
 principal_id uuid PRIMARY KEY REFERENCES principals(id) ON DELETE RESTRICT,
 encrypted_refresh bytea NOT NULL,
 scopes text[] NOT NULL,
 state text NOT NULL DEFAULT 'active' CHECK(state IN('active','revoked')),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE delegation_authorizations (
 state_hash bytea PRIMARY KEY CHECK(octet_length(state_hash)=32),
 principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 encrypted_verifier bytea NOT NULL,
 expires_at timestamptz NOT NULL
);
