<script lang="ts">
  import Icon from '../Icon.svelte';

  let { tags, suggestions = [], readonly = false, onSave }: {
    tags: string[];
    suggestions?: string[];
    readonly?: boolean;
    onSave: (tags: string[]) => Promise<void>;
  } = $props();

  let draft = $state('');
  let busy = $state(false);
  let error = $state('');
  const listId = `tag-suggestions-${Math.random().toString(36).slice(2, 8)}`;
  const options = $derived(suggestions.filter((tag) => !tags.some((own) => own.toLowerCase() === tag.toLowerCase())));

  async function save(next: string[]) {
    busy = true;
    error = '';
    try {
      await onSave(next);
      draft = '';
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Could not save tags';
    } finally {
      busy = false;
    }
  }

  function add() {
    const added = draft.split(',').map((tag) => tag.trim().replace(/\s+/g, ' ')).filter(Boolean);
    if (added.length) void save([...tags, ...added]);
  }

  function keydown(event: KeyboardEvent) {
    if (event.key === 'Enter' || event.key === ',') {
      event.preventDefault();
      add();
    } else if (event.key === 'Backspace' && draft === '' && tags.length) {
      void save(tags.slice(0, -1));
    }
  }
</script>

<div class="tags" class:busy>
  {#each tags as tag (tag)}
    <span class="tag">{tag}{#if !readonly}<button type="button" aria-label={`Remove tag ${tag}`} disabled={busy} onclick={() => void save(tags.filter((own) => own !== tag))}><Icon name="x" size={10} /></button>{/if}</span>
  {/each}
  {#if !readonly}
    <input type="text" list={listId} maxlength="32" placeholder={tags.length ? 'Add' : 'Add tag'} aria-label="Add tag" bind:value={draft} disabled={busy} onkeydown={keydown} onblur={() => { if (draft.trim()) add(); }} />
    <datalist id={listId}>{#each options as option (option)}<option value={option}></option>{/each}</datalist>
  {:else if tags.length === 0}
    <span class="none">—</span>
  {/if}
</div>
{#if error}<p class="form-error" role="alert">{error}</p>{/if}

<style>
  .tags { display: flex; flex-wrap: wrap; align-items: center; gap: 4px; min-width: 0; }
  .tags.busy { opacity: 0.6; }
  .tag { display: inline-flex; align-items: center; gap: 2px; padding: 0 4px 0 8px; border: 1px solid var(--line); border-radius: var(--radius-pill); background: var(--surface-muted, var(--surface)); font-size: 12px; font-weight: 550; line-height: 20px; }
  .tag button { display: inline-grid; place-items: center; width: 16px; height: 16px; padding: 0; border: 0; border-radius: 50%; background: transparent; color: var(--muted); cursor: pointer; }
  .tag button:hover { background: var(--line); color: var(--ink); }
  input { width: 84px; min-width: 0; padding: 1px 6px; border: 1px dashed var(--line-strong); border-radius: var(--radius-pill); background: transparent; color: var(--ink); font: inherit; font-size: 12px; line-height: 18px; }
  input:focus { border-style: solid; outline: none; border-color: var(--accent); }
  .none { color: var(--muted); }
  .form-error { margin: 4px 0 0; font-size: 12px; }
</style>
