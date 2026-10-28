<script lang="ts">
  import Icon from '../Icon.svelte';

  let {
    open = false,
    title = 'Please wait',
    message = 'Processing your request…'
  }: {
    open: boolean;
    title?: string;
    message?: string;
  } = $props();
</script>

{#if open}
  <div class="loading-backdrop" role="dialog" aria-modal="true" aria-busy="true">
    <div class="loading-card">
      <div class="loading-icon-badge">
        <Icon name="loader" size={22} class="spin" />
      </div>
      <div class="loading-text">
        <h3 class="loading-title">{title}</h3>
        {#if message}
          <p class="loading-message">{message}</p>
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .loading-backdrop {
    position: fixed;
    inset: 0;
    z-index: 10000;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 1.5rem;
    background: rgba(5, 9, 15, 0.65);
    backdrop-filter: blur(4px);
    -webkit-backdrop-filter: blur(4px);
    animation: fade-in var(--dur, 0.15s) var(--ease-out, ease-out);
  }
  :global(:root[data-theme='dark']) .loading-backdrop {
    background: rgba(0, 0, 0, 0.72);
  }
  .loading-card {
    position: relative;
    display: flex;
    align-items: center;
    gap: 16px;
    width: 100%;
    max-width: 440px;
    padding: 22px 24px;
    border: 1px solid var(--line);
    border-radius: var(--radius-xl);
    background: var(--surface-elevated);
    box-shadow: var(--shadow-lg);
    animation: modal-in var(--dur-slow, 0.25s) var(--ease-spring, cubic-bezier(0.16, 1, 0.3, 1));
  }
  .loading-card::before {
    content: '';
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    height: 3px;
    background: var(--accent);
    border-top-left-radius: var(--radius-xl);
    border-top-right-radius: var(--radius-xl);
  }
  .loading-icon-badge {
    display: inline-flex;
    flex-shrink: 0;
    align-items: center;
    justify-content: center;
    width: 44px;
    height: 44px;
    border-radius: var(--radius-lg);
    background: var(--accent-bg);
    color: var(--accent);
  }
  .loading-text {
    display: grid;
    gap: 4px;
    min-width: 0;
  }
  .loading-title {
    margin: 0;
    font-size: 15px;
    font-weight: 600;
    color: var(--ink);
    line-height: 1.3;
  }
  .loading-message {
    margin: 0;
    font-size: 13px;
    color: var(--muted);
    line-height: 1.45;
  }
  @keyframes fade-in {
    from { opacity: 0; }
    to { opacity: 1; }
  }
  @keyframes modal-in {
    from { opacity: 0; transform: scale(0.96) translateY(8px); }
    to { opacity: 1; transform: none; }
  }
</style>
