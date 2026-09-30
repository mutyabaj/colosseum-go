CREATE TABLE IF NOT EXISTS grants (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  funder_name TEXT NOT NULL,
  program_name TEXT,
  category TEXT NOT NULL DEFAULT 'general',
  source TEXT,
  amount TEXT,
  deadline TEXT,
  eligibility_keywords TEXT,
  alignment_score INTEGER,
  link TEXT,
  contact_name TEXT,
  contact_email TEXT,
  notes TEXT,
  draft_narrative TEXT,
  application_draft_artifact_path TEXT,
  loi_draft_status TEXT NOT NULL DEFAULT '',
  application_draft_status TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'new',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_grants_funder_program ON grants(funder_name, program_name);
CREATE INDEX IF NOT EXISTS idx_grants_run_id ON grants(run_id);
CREATE INDEX IF NOT EXISTS idx_grants_category ON grants(category);
