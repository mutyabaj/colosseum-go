-- MCP (Model Context Protocol) server registry.
-- Each row is a registered MCP server; agents opt into servers via agent_mcp_servers.
CREATE TABLE IF NOT EXISTS mcp_servers (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL UNIQUE,           -- human-readable, used to build tool prefix mcp__{slug}__
  description     TEXT NOT NULL DEFAULT '',
  transport       TEXT NOT NULL DEFAULT 'stdio',  -- 'stdio' or 'http'
  -- stdio transport
  command         TEXT NOT NULL DEFAULT '',       -- executable path or name
  args_json       TEXT NOT NULL DEFAULT '[]',     -- JSON array of string args
  env_json        TEXT NOT NULL DEFAULT '{}',     -- extra env vars injected into the process
  -- http transport
  url             TEXT NOT NULL DEFAULT '',       -- MCP endpoint URL
  headers_json    TEXT NOT NULL DEFAULT '{}',     -- request headers (e.g. Authorization)
  -- shared
  timeout_seconds INTEGER NOT NULL DEFAULT 30,
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

-- Many-to-many: which MCP servers each agent may use.
CREATE TABLE IF NOT EXISTS agent_mcp_servers (
  agent_id      TEXT NOT NULL,
  mcp_server_id TEXT NOT NULL,
  PRIMARY KEY (agent_id, mcp_server_id)
);
