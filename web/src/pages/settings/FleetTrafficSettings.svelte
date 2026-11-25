<script lang="ts">
  import { ApiError, apiClient } from '../../api';
  import { PREVIEW_MODE } from '../../lib/env';

  let { isOwner, onAuthExpired }: { isOwner: boolean; onAuthExpired: () => void } = $props();
  let enabled = $state<boolean | null>(null);
  let busy = $state(false);
  let error = $state('');
  let retry = $state(0);

  function showError(cause: unknown) {
    if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
    error = cause instanceof ApiError ? cause.message : 'Fleet traffic settings could not be updated.';
  }

  $effect(() => {
    if (PREVIEW_MODE) return;
    const controller = new AbortController();
    function load(attempt: number) {
      if (attempt) error = '';
      void apiClient.getFleetTrafficSettings(controller.signal).then(
        (result) => { if (!controller.signal.aborted) enabled = result.enabled; },
        (cause) => { if (!controller.signal.aborted) showError(cause); }
      );
    }
    load(retry);
    return () => controller.abort();
  });

  async function setEnabled(next: boolean) {
    busy = true;
    error = '';
    try {
      enabled = (await apiClient.saveFleetTrafficSettings(next)).enabled;
    } catch (cause) {
      showError(cause);
    } finally {
      busy = false;
    }
  }
</script>

<article class="panel">
  <div class="panel-heading">
    <div>
      <h2>Fleet traffic history</h2>
      <span class="muted">Save hourly fleet totals on the hub for 30 days. Turning saving off leaves existing hours available until they expire.</span>
    </div>
    {#if !PREVIEW_MODE && enabled !== null}
      <button class="button ghost small" type="button" role="switch" aria-checked={enabled} disabled={!isOwner || busy} onclick={() => void setEnabled(!enabled)}>
        Save fleet traffic history <strong>{enabled ? 'On' : 'Off'}</strong>
      </button>
    {/if}
  </div>
  {#if PREVIEW_MODE}
    <p class="muted">Available on the hub.</p>
  {:else if error}
    <p role="alert">{error} {#if enabled === null}<button class="button ghost small" type="button" onclick={() => retry++}>Retry</button>{/if}</p>
  {:else if enabled === null}
    <p class="muted" role="status">Loading fleet traffic settings…</p>
  {:else if !isOwner}
    <p class="muted">The hub owner can change this setting.</p>
  {/if}
</article>

<style>
  .panel { margin-bottom: 16px; }
  .panel-heading { flex-wrap: wrap; gap: 12px; }
  .button strong { margin-left: 4px; }
</style>
