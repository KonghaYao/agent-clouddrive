-- Separate controlled audit entry point; never written by feedback submission.
CREATE TABLE audit_reads (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
 version_id uuid REFERENCES file_versions(id) ON DELETE RESTRICT,
 artifact_set_id uuid REFERENCES artifact_sets(id) ON DELETE RESTRICT,
 granted_by uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 read_kind text NOT NULL CHECK(read_kind IN ('versions','original','markdown','asset')),
 read_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_reads_subject_file ON audit_reads(principal_id,file_id,read_at);
