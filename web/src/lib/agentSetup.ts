export type AgentId = 'claude' | 'codex' | 'gemini' | 'vscode' | 'json' | 'curl';

export const agents: { id: AgentId; label: string; hint: string }[] = [
  { id: 'claude', label: 'Claude Code', hint: 'Run in a terminal.' },
  { id: 'codex', label: 'Codex', hint: 'Run in a terminal. Adds the server to ~/.codex/config.toml.' },
  { id: 'gemini', label: 'Gemini CLI', hint: 'Run in a terminal.' },
  { id: 'vscode', label: 'VS Code', hint: 'Run in a terminal with the code command installed.' },
  { id: 'json', label: 'Other (JSON)', hint: 'For Claude Desktop, Cursor, and other clients that take an mcpServers config.' },
  { id: 'curl', label: 'REST (curl)', hint: 'Call the REST API directly.' }
];

/** The placeholder shown when the real secret is not available. */
export const TOKEN_PLACEHOLDER = 'YOUR_TOKEN';

const quote = (value: string): string => `'${value.replace(/'/g, `'\\''`)}'`;

/** Builds the setup command for an agent; origin is the dashboard's https origin. */
export function agentCommand(agent: AgentId, origin: string, token: string): string {
  const url = `${origin.replace(/\/+$/, '')}/api/v1/mcp`;
  const auth = `Bearer ${token || TOKEN_PLACEHOLDER}`;
  switch (agent) {
    case 'claude':
      return `claude mcp add --transport http payesh ${url} --header "Authorization: ${auth}"`;
    case 'codex':
      return `mkdir -p ~/.codex && printf '\\n[mcp_servers.payesh]\\nurl = "${url}"\\nhttp_headers = { Authorization = "${auth}" }\\n' >> ~/.codex/config.toml`;
    case 'gemini':
      return `gemini mcp add --transport http payesh ${url} --header "Authorization: ${auth}"`;
    case 'vscode':
      return `code --add-mcp ${quote(JSON.stringify({ name: 'payesh', type: 'http', url, headers: { Authorization: auth } }))}`;
    case 'json':
      return JSON.stringify({ mcpServers: { payesh: { type: 'http', url, headers: { Authorization: auth } } } }, null, 2);
    case 'curl':
      return `curl -H "Authorization: ${auth}" ${url.replace(/\/mcp$/, '/servers')}`;
  }
}
