<script lang="ts">
  // Shows that numbers refresh by themselves: a pulsing dot while live, a
  // spinning ring during a refresh, and how old the data is.
  let { refreshing = false, updatedAt = null }: { refreshing?: boolean; updatedAt?: number | null } = $props();
  let now = $state(Date.now());
  $effect(() => {
    const timer = window.setInterval(() => (now = Date.now()), 1000);
    return () => window.clearInterval(timer);
  });
  const age = $derived(updatedAt ? Math.max(0, Math.round((now - updatedAt) / 1000)) : null);
</script>

<span class="live" class:refreshing role="status" aria-live="polite" title="Updates automatically">
  <span class="mark" aria-hidden="true"></span>
  {#if refreshing}Updating…{:else if age !== null}Live · {age < 2 ? 'just now' : `${age}s ago`}{:else}Loading…{/if}
</span>

<style>
  .live { display: inline-flex; align-items: center; gap: 7px; color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .mark { position: relative; width: 8px; height: 8px; border-radius: 50%; background: var(--success, #3fb950); }
  .mark::after { content: ''; position: absolute; inset: -3px; border-radius: 50%; border: 2px solid var(--success, #3fb950); opacity: 0; animation: pulse 2.4s ease-out infinite; }
  .refreshing .mark { background: transparent; border: 2px solid var(--line-strong, var(--line)); border-top-color: var(--accent); animation: spin 0.8s linear infinite; }
  .refreshing .mark::after { display: none; }
  @keyframes pulse { 0% { transform: scale(0.6); opacity: 0.7; } 100% { transform: scale(1.6); opacity: 0; } }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .mark::after, .refreshing .mark { animation: none; } }
</style>
