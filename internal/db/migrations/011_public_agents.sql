-- Public-facing agents: is_public enables unauthenticated chat access,
-- public_token is a scoped credential embedded in the public chat page,
-- custom_domain allows vita.mnequivoicepartnership.org → /c/:id redirect.
ALTER TABLE agents ADD COLUMN is_public INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN public_token TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN custom_domain TEXT NOT NULL DEFAULT '';
