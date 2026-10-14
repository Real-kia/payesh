<script lang="ts">
  import Icon from '../Icon.svelte';
  import { agentCommand, agents, type AgentId } from '../lib/agentSetup';

  // token is the one-time secret right after creation; without it, the user can
  // paste an existing key. The key stays in this page and is never sent anywhere.
  let { token = '', onCopyError }: { token?: string; onCopyError: (message: string) => void } = $props();

  const origin = typeof window === 'undefined' ? '' : window.location.origin;
  let agent = $state<AgentId>('claude');
  let pasted = $state('');
  let copied = $state(false);

  const selected = $derived(agents.find((item) => item.id === agent) ?? agents[0]);
  const command = $derived(agentCommand(agent, origin, token || pasted.trim()));

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(command);
      copied = true;
      setTimeout(() => (copied = false), 1600);
    } catch {
      onCopyError('Copy failed. Select the text and copy it manually.');
    }
  }
</script>

<div class="agent-setup">
  <div class="agent-picker" role="radiogroup" aria-label="AI agent">
    {#each agents as item (item.id)}
      <button type="button" role="radio" aria-checked={agent === item.id} class:active={agent === item.id} onclick={() => { agent = item.id; copied = false; }}>{item.label}</button>
    {/each}
  </div>
  {#if !token}
    <label class="paste">API key<input type="password" bind:value={pasted} placeholder="Paste a pyt_… key to fill it in, or leave YOUR_TOKEN" autocomplete="off" spellcheck="false" /></label>
  {/if}
  <div class="terminal-block">
    <code>{command}</code>
    <button class="icon-button" type="button" aria-label={`Copy ${selected.label} setup`} title={copied ? 'Copied' : 'Copy'} onclick={() => void copy()}><Icon name={copied ? 'check' : 'copy'} size={14} /></button>
  </div>
  <p class="muted hint">{selected.hint}</p>
</div>

<style>
  .agent-setup { display: grid; gap: 10px; min-width: 0; }
  .agent-picker { display: flex; flex-wrap: wrap; gap: 6px; }
  .agent-picker button { padding: 6px 12px; border: 1px solid var(--line); border-radius: 999px; background: var(--surface); color: var(--ink-secondary); font: inherit; font-size: 12.5px; font-weight: 600; cursor: pointer; }
  .agent-picker button:hover { border-color: var(--accent); color: var(--ink); }
  .agent-picker button.active { border-color: var(--accent); background: var(--accent); color: var(--accent-contrast); }
  .paste { display: grid; gap: 6px; max-width: 420px; }
  .hint { margin: 0; font-size: 12px; }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; min-width: 0; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; flex-shrink: 0; border-color: transparent; background: rgba(255, 255, 255, .07); color: var(--term-ink); }
</style>
