<script lang="ts" generics="T extends string">
  import Icon from '../Icon.svelte';

  // A two-way "how do you want to do this" choice. Only the chosen option's
  // fields are shown below it, so the page never lists every option at once.
  type Option = { value: T; label: string; hint: string; icon: string };
  let { value = $bindable(), options, label }: { value: T; options: Option[]; label: string } = $props();
</script>

<div class="method-switch" role="radiogroup" aria-label={label}>
  {#each options as option (option.value)}
    <button type="button" role="radio" aria-checked={value === option.value} class:active={value === option.value} onclick={() => (value = option.value)}>
      <span class="icon"><Icon name={option.icon} size={18} /></span>
      <span class="text"><strong>{option.label}</strong><span>{option.hint}</span></span>
    </button>
  {/each}
</div>

<style>
  .method-switch { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 12px; }
  button { display: flex; align-items: flex-start; gap: 12px; min-height: 44px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--ink); font: inherit; text-align: left; cursor: pointer; transition: border-color 150ms, background-color 150ms; }
  button:hover { border-color: var(--ink-secondary); }
  button.active { border-color: var(--accent); background: var(--accent-bg); }
  .icon { display: inline-flex; margin-top: 1px; color: var(--ink-secondary); }
  button.active .icon { color: var(--accent); }
  .text { display: grid; gap: 2px; min-width: 0; }
  .text strong { font-size: 14px; font-weight: 600; }
  .text span { font-size: 13px; line-height: 1.4; color: var(--ink-secondary); }
  @media (prefers-reduced-motion: reduce) { button { transition: none; } }
</style>
