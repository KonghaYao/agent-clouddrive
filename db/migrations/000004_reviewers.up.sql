-- Explicit review grants, never inferred from a generic administrator role.
CREATE TABLE reviewer_grants (
 principal_id uuid PRIMARY KEY REFERENCES principals(id) ON DELETE RESTRICT,
 can_view_jobs boolean NOT NULL DEFAULT false,
 can_triage_feedback boolean NOT NULL DEFAULT false,
 granted_by uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 granted_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE audit_grants (
 principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
 granted_by uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 expires_at timestamptz,
 PRIMARY KEY(principal_id,file_id)
);
CREATE TABLE conversion_requests (
 principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 job_id uuid NOT NULL REFERENCES conversion_jobs(id) ON DELETE RESTRICT,
 PRIMARY KEY(principal_id,idempotency_key)
);
