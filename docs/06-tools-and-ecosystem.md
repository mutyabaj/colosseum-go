# Tools and Ecosystem

This guide covers tool architecture, runtime behavior, and ecosystem entities.

## Tool Registry Model

Definitions live in `tool_defs` with fields such as:

- `name` (unique identifier)
- `description`
- `input_schema_json`
- `kind` (`builtin`, `shell_command`, ...)
- `config_json`
- `enabled`
- `is_builtin`

Built-ins are upserted at startup and treated as immutable in UI/API.

## Built-in Tool Set

### Shell and Filesystem

- `shell.exec`
- `file.read`
- `file.read_range`
- `file.write`
- `file.search`
- `file.exists`
- `file.stat`
- `file.list`
- `path.glob`
- `apply.patch`

### Web and JSON

- `web.fetch`
- `http.request`
- `json.parse`
- `json.query`

### Browser

- `browser.open`
- `browser.snapshot`
- `browser.action`
- `browser.wait`
- `browser.close`
- `browser.screenshot`

### Run Artifacts and Utility

- `artifact.list`
- `artifact.get`
- `recall_artifact`
- `test.run`
- `clock.now`
- `env.inspect`
- `approval.request`

### Session Memory and Process Control

- `scratchpad.write`
- `scratchpad.read`
- `scratchpad.delete`
- `process.run_background`
- `process.logs`
- `process.kill`

### Delegation and Planning

- `subagent.spawn`
- `subagent.status`
- `subagent.wait`
- `plan.set`
- `plan.update_step`
- `plan.update_steps`
- `plan.add_step`
- `plan.read`

## Browser Runtime Details

Browser tools are single-session-per-run and support:

- Docker-backed Playwright by default
- optional local fallback
- screenshot artifact persistence

Recommended operation:

- pin `COLOSSEUM_BROWSER_IMAGE` to the exact Playwright version in use
- keep fallback enabled for resilience during local/dev setups

## Custom Tools

Current custom kind:

- `shell_command`

Key config:

- `command_template` with `{{param}}` placeholders
- `timeout_seconds`

Treat custom shell templates as privileged: placeholder values are rendered into a shell command string, so only trusted operators should create or enable them.

Execution flow:

1. resolve definition by tool name
2. render template from input args
3. execute in run workspace context
4. return output/log artifacts

## Tool Governance

Tool execution is controlled by:

- agent-level `allowed_tools`
- policy engine evaluation (`allow`, `deny`, `require approval`)
- tool-specific guardrails (for example host/scheme checks for web/browser tools)

## Tool Testing

Use tool test runner (`POST /api/tools/{id}/test`) or UI test console.

Input:

- workspace path
- JSON tool input

Output:

- `ok`
- `output`
- `log`
- optional `error`

## MCP (Model Context Protocol) Servers

Colosseum agents can connect to external MCP servers at run time, exposing their tools to the LLM alongside built-in tools. The implementation lives in `internal/mcp/`.

### How It Works

When an agent run starts, Colosseum:

1. Queries the database for all MCP servers assigned to that agent (`agent_mcp_servers` join `mcp_servers`)
2. Connects to each enabled server
3. Calls `tools/list` to discover what tools the server exposes
4. Prefixes every tool name as `mcp__{serverSlug}__{toolName}` and adds them to the LLM's tool list
5. Routes any `mcp__`-prefixed tool call to the correct server during the agent loop
6. Closes all connections when the run ends

### Tool Naming

MCP tools follow the convention:

```
mcp__{server_slug}__{local_tool_name}
```

`server_slug` is the server's `name` field lowercased with non-alphanumeric characters replaced by underscores. For example, a server named `"My Files"` with a tool `read_file` becomes `mcp__my_files__read_file`.

### Transports

Two transports are supported, configured per server via the `transport` field:

**stdio** (default)

Spawns the MCP server as a local subprocess and communicates via newline-delimited JSON-RPC 2.0 over stdin/stdout. Used for locally installed MCP servers such as `npx @modelcontextprotocol/server-filesystem`.

Config fields used: `command`, `args_json`, `env_json`

**http**

Sends all JSON-RPC messages as HTTP POST requests to a single endpoint URL. Used for remote or hosted MCP servers.

Config fields used: `url`, `headers_json`, `timeout_seconds`

### Database Schema

| Table | Purpose |
|---|---|
| `mcp_servers` | One row per MCP server: name, transport, command/URL, credentials, timeout |
| `agent_mcp_servers` | Join table assigning servers to agents (many-to-many) |

### Adding an MCP Server

1. Insert a row into `mcp_servers` with `enabled = 1` and the appropriate transport config
2. Insert a row into `agent_mcp_servers` linking the server to the target agent
3. On the next run for that agent, the server will be connected and its tools available automatically — no restart required

### Governance

MCP tools participate in the same policy engine as built-in tools. Add `mcp__{slug}__*` patterns to an agent's `allowed_tools` or policy rules to control access.

### Failure Handling

If a server fails to connect or `tools/list` returns an error, that server is skipped and logged. The run continues with the remaining servers. Individual tool call failures return an `[mcp error]`-prefixed string to the LLM rather than aborting the run.

---

## Ecosystem Resources

### Policies

- stored in `policies`
- evaluated before tool execution

### Secrets

- stored in `secrets`
- list endpoints do not return secret plaintext
- creating or updating secrets requires `COLOSSEUM_SECRET_KEY`

### Provider Configs

- stored in `provider_configs`
- used for provider profile management and future routing patterns

## Practical Rollout Pattern

1. enable minimum required tools for each agent
2. test custom tools in isolation
3. apply policy gates for risky operations
4. monitor run telemetry and artifacts
5. iterate based on transcript/debug evidence

