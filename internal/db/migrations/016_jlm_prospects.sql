CREATE TABLE IF NOT EXISTS jlm_prospects (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  organization_name TEXT NOT NULL,
  ein TEXT,
  location TEXT,
  mission TEXT,
  annual_revenue TEXT,
  latest_990_year TEXT,
  latest_990_summary TEXT,
  website TEXT,
  executive_director TEXT,
  contact_email TEXT,
  contact_phone TEXT,
  accounting_indicators TEXT,
  why_prospect TEXT,
  draft_outreach_subject TEXT,
  draft_outreach_body TEXT,
  status TEXT NOT NULL DEFAULT 'researched',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_jlm_prospects_org_name ON jlm_prospects(organization_name);
CREATE INDEX IF NOT EXISTS idx_jlm_prospects_run_id ON jlm_prospects(run_id);
