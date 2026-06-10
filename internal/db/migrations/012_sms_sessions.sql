-- Maps an inbound phone number + agent to a persistent chat session.
-- One row per (phone_number, agent_id) pair so each caller gets continuity.
CREATE TABLE IF NOT EXISTS sms_sessions (
  phone_number TEXT NOT NULL,
  agent_id     TEXT NOT NULL,
  session_id   TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (phone_number, agent_id)
);
