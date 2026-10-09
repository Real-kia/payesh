<script lang="ts" module>
  export type Command = { id: string; label: string; hint?: string; icon: string; group: string; keywords?: string; run: () => void };
</script>

<script lang="ts">
  import { tick } from 'svelte';
  import Icon from '../Icon.svelte';
  import { stateLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { open = $bindable(false), commands, servers, onServer }: {
    open?: boolean;
    commands: Command[];
    servers: PreviewServer[];
    onServer: (server: PreviewServer) => void;
  } = $props();

  let dialog: HTMLDialogElement;
  let input: HTMLInputElement;
  let query = $state('');
  let active = $state(0);

  const all = $derived<Command[]>([
    ...commands,
    ...servers.map((server) => ({ id: `server:${server.id}`, label: server.name, hint: stateLabel(server.displayState), icon: 'server', group: 'Servers', keywords: `${server.address ?? ''} ${server.platform} ${server.role}`, run: () => onServer(server) }))
  ]);

  // Every word of the query must appear in the label, group or keywords.
  const results = $derived.by(() => {
    const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
    const matched = words.length ? all.filter((command) => { const text = `${command.label} ${command.group} ${command.keywords ?? ''}`.toLowerCase(); return words.every((word) => text.includes(word)); }) : all;
    return matched.slice(0, 40);
  });

  $effect(() => {
    if (open && dialog && !dialog.open) {
      query = '';
      active = 0;
      dialog.showModal();
      void tick().then(() => input?.focus());
    } else if (!open && dialog?.open) dialog.close();
  });
  $effect(() => { void query; active = 0; });

  function choose(command: Command | undefined) {
    if (!command) return;
    open = false;
    command.run();
  }

  function onKey(event: KeyboardEvent) {
    if (event.key === 'ArrowDown') { event.preventDefault(); active = Math.min(results.length - 1, active + 1); scrollActive(); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); active = Math.max(0, active - 1); scrollActive(); }
    else if (event.key === 'Enter') { event.preventDefault(); choose(results[active]); }
  }

  function scrollActive() {
    void tick().then(() => dialog?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' }));
  }
</script>

<dialog bind:this={dialog} class="palette" aria-label="Go to" onclose={() => (open = false)} onclick={(event) => { if (event.target === dialog) open = false; }}>
  <div class="box">
    <div class="search">
      <Icon name="search" size={16} />
      <input bind:this={input} bind:value={query} onkeydown={onKey} placeholder="Go to a page, server or setting…" aria-label="Search pages, servers and settings" role="combobox" aria-expanded="true" aria-controls="palette-results" aria-activedescendant={results[active] ? `palette-${results[active].id}` : undefined} autocomplete="off" spellcheck="false" />
      <kbd>Esc</kbd>
    </div>
    <ul id="palette-results" class="results" role="listbox">
      {#each results as command, index (command.id)}
        {#if index === 0 || results[index - 1].group !== command.group}<li class="group" role="presentation">{command.group}</li>{/if}
        <li id={`palette-${command.id}`} role="option" aria-selected={index === active} class:active={index === active} onmousemove={() => (active = index)} onclick={() => choose(command)} onkeydown={() => {}}>
          <span class="icon"><Icon name={command.icon} size={15} /></span>
          <span class="label">{command.label}</span>
          {#if command.hint}<span class="hint">{command.hint}</span>{/if}
          <Icon name="arrow-right" size={13} class="go" />
        </li>
      {:else}
        <li class="empty" role="presentation">Nothing matches “{query}”.</li>
      {/each}
    </ul>
    <div class="foot"><span><kbd>↑</kbd><kbd>↓</kbd> move</span><span><kbd>Enter</kbd> open</span><span><kbd>⌘</kbd><kbd>K</kbd> toggle</span></div>
  </div>
</dialog>

<style>
  .palette { width: min(560px, calc(100vw - 32px)); max-width: none; max-height: none; margin: 12vh auto auto; padding: 0; overflow: visible; border: 0; background: transparent; color: inherit; }
  .palette::backdrop { background: rgba(5, 9, 15, 0.45); animation: fade-in var(--dur-fast) var(--ease-out); }
  .box { overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius-xl); background: var(--surface-elevated); box-shadow: var(--shadow-lg); animation: pop-in var(--dur) var(--ease-out); }
  .search { display: flex; align-items: center; gap: 10px; padding: 0 14px; border-bottom: 1px solid var(--line); color: var(--muted); }
  .search input { flex: 1; padding: 15px 0; border: 0; background: transparent; box-shadow: none; font-size: 15px; }
  .search input:focus { box-shadow: none; }
  kbd { display: inline-grid; place-items: center; min-width: 20px; padding: 1px 5px; border: 1px solid var(--line); border-bottom-width: 2px; border-radius: 4px; background: var(--surface-muted); color: var(--muted); font-family: var(--font-sans); font-size: 10.5px; }
  .results { max-height: min(52vh, 420px); margin: 0; padding: 6px; overflow-y: auto; list-style: none; }
  .group { padding: 10px 10px 4px; color: var(--muted); font-size: 10.5px; font-weight: 700; letter-spacing: 0.07em; text-transform: uppercase; }
  [role='option'] { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border-radius: var(--radius-md); color: var(--ink-secondary); font-size: 13.5px; cursor: pointer; }
  [role='option'].active { background: var(--accent-bg); color: var(--ink); }
  .icon { display: grid; place-items: center; width: 26px; height: 26px; border-radius: var(--radius-sm); background: var(--surface-muted); color: var(--muted); }
  .active .icon { background: var(--surface); color: var(--accent); }
  .label { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .hint { color: var(--muted); font-size: 12px; text-transform: capitalize; }
  [role='option'] :global(.go) { opacity: 0; color: var(--accent); transition: opacity var(--transition-fast), transform var(--transition-fast); transform: translateX(-4px); }
  [role='option'].active :global(.go) { opacity: 1; transform: none; }
  .empty { padding: 26px 10px; color: var(--muted); text-align: center; font-size: 13px; }
  .foot { display: flex; gap: 16px; padding: 8px 14px; border-top: 1px solid var(--line); background: var(--surface-muted); color: var(--muted); font-size: 11.5px; }
  .foot span { display: inline-flex; align-items: center; gap: 4px; }
  @media (max-width: 640px) { .palette { margin-top: 8vh; } .foot { display: none; } }
</style>
