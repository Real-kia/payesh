import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentCommand, agents } from '../src/lib/agentSetup.ts';

const origin = 'https://mon.example.com/';

test('every agent command targets the hub MCP endpoint and carries the token', () => {
  for (const { id } of agents) {
    const command = agentCommand(id, origin, 'pyt_abc');
    assert.match(command, /https:\/\/mon\.example\.com\/api\/v1\/(mcp|servers)/);
    assert.match(command, /Bearer pyt_abc/);
  }
});

test('a missing token falls back to the placeholder', () => {
  assert.match(agentCommand('claude', origin, ''), /Bearer YOUR_TOKEN"$/);
});

test('codex appends a streamable HTTP server with a static header', () => {
  assert.equal(
    agentCommand('codex', origin, 'pyt_abc'),
    `mkdir -p ~/.codex && printf '\\n[mcp_servers.payesh]\\nurl = "https://mon.example.com/api/v1/mcp"\\nhttp_headers = { Authorization = "Bearer pyt_abc" }\\n' >> ~/.codex/config.toml`
  );
});

test('vscode passes valid JSON as one shell argument', () => {
  const command = agentCommand('vscode', origin, 'pyt_abc');
  const json = command.replace(/^code --add-mcp '/, '').replace(/'$/, '');
  assert.deepEqual(JSON.parse(json), { name: 'payesh', type: 'http', url: 'https://mon.example.com/api/v1/mcp', headers: { Authorization: 'Bearer pyt_abc' } });
});
