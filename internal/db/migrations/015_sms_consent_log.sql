-- Audit log for SMS consent collected during voice calls.
-- Each row records one consent event with evidence for A2P 10DLC compliance.
CREATE TABLE IF NOT EXISTS sms_consent_log (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  call_sid         TEXT NOT NULL,
  phone_number     TEXT NOT NULL,
  consented        INTEGER NOT NULL DEFAULT 0, -- 1 = yes, 0 = no/timeout
  consent_method   TEXT NOT NULL DEFAULT '',   -- 'dtmf' or 'speech'
  consent_response TEXT NOT NULL DEFAULT '',   -- raw digit or speech result
  consented_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sms_consent_log_call ON sms_consent_log(call_sid);
CREATE INDEX IF NOT EXISTS idx_sms_consent_log_phone ON sms_consent_log(phone_number);
