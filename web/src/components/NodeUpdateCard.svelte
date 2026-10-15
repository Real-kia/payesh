<script lang="ts">
  import Icon from '../Icon.svelte';
  import { elapsedLabel, failureMessage, isActive, phaseOf, phaseSteps, type NodeUpdateTrack } from '../lib/nodeUpdate';
  import { nodeVersionOf } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { server, latestNode, latestRelease, track, now, canUpdate, busy, error, jobError, onUpdate, onDismiss }: {
    server: PreviewServer;
    latestNode: string;
    latestRelease: string;
    track: NodeUpdateTrack | null;
    now: number;
    canUpdate: boolean;
    busy: boolean;
    error: string;
    jobError?: { code?: string; message?: string };
    onUpdate: () => void;
    onDismiss: () => void;
  } = $props();

  const installed = $derived(nodeVersionOf(server));
  const parse = (value: string) => value.split(/[-+]/)[0].split('.').map((part) => Number.parseInt(part, 10) || 0);
  const outdated = $derived.by(() => {
    if (!latestNode || !/^\d+\.\d+\.\d+/.test(installed) || !/^\d+\.\d+\.\d+/.test(latestNode)) return false;
    const a = parse(installed), b = parse(latestNode);
    for (let i = 0; i < Math.max(a.length, b.length); i++) if ((a[i] ?? 0) !== (b[i] ?? 0)) return (a[i] ?? 0) < (b[i] ?? 0);
    return false;
  });
  const phase = $derived(track ? phaseOf(track, server, now) : null);
  const activeIndex = $derived(phase ? phaseSteps.findIndex((step) => step.phase === phase) : -1);
  const hubOnly = $derived(!!latestRelease && !!latestNode && latestRelease !== latestNode);
</script>

<section class="node-update" class:updating={phase && isActive(phase)} class:failed={phase === 'failed' || phase === 'stalled'} class:done={phase === 'done'} aria-live="polite" aria-label="Node agent version and update">
  {#if track && phase}
    <div class="head">
      <span class="badge"><Icon name={phase === 'done' ? 'check' : phase === 'failed' || phase === 'stalled' ? 'alert-triangle' : 'refresh'} size={15} /></span>
      <div class="title">
        {#if phase === 'done'}
          <strong>Updated to v{track.release}</strong>
          <span>The node restarted and reports the new version. Took {elapsedLabel(now - track.startedAt)}.</span>
        {:else if phase === 'failed'}
          <strong>The update did not finish</strong>
          <span>{failureMessage(jobError ?? (track.error ? { message: track.error } : undefined), track.jobState)} The node keeps running v{installed}.</span>
        {:else if phase === 'stalled'}
          <strong>This is taking longer than expected</strong>
          <span>No confirmation after {elapsedLabel(now - track.startedAt)}. The node may be offline or still restarting. It keeps running its previous version until it confirms.</span>
        {:else}
          <strong>Updating v{track.from} → v{track.release}</strong>
          <span>{phaseSteps[activeIndex]?.detail} Elapsed {elapsedLabel(now - track.startedAt)}.</span>
        {/if}
      </div>
      {#if phase === 'done' || phase === 'failed' || phase === 'stalled'}
        <div class="actions">
          {#if phase !== 'done' && canUpdate}<button class="button primary small" type="button" disabled={busy} onclick={onUpdate}><Icon name="refresh" size={13} /><span>Try again</span></button>{/if}
          <button class="button ghost small" type="button" onclick={onDismiss}>Dismiss</button>
        </div>
      {/if}
    </div>
    {#if phase !== 'failed' && phase !== 'stalled' || activeIndex >= 0}
      <ol class="steps" aria-label="Update progress">
        {#each phaseSteps as step, index (step.phase)}
          <li class:current={phase !== 'done' && index === activeIndex} class:complete={phase === 'done' || index < activeIndex}>
            <span class="dot">{#if phase === 'done' || index < activeIndex}<Icon name="check" size={11} />{:else if index === activeIndex}<i class="spinner"></i>{/if}</span>
            <span class="label">{step.label}</span>
          </li>
        {/each}
      </ol>
    {/if}
  {:else}
    <div class="head">
      <span class="badge" class:ok={!outdated}><Icon name={outdated ? 'download' : 'check'} size={15} /></span>
      <div class="title">
        {#if outdated}
          <strong>Node agent v{installed} · update available</strong>
          <span>v{latestNode} is the newest node agent. The update installs the signed release and restarts the node, which goes offline briefly.</span>
        {:else}
          <strong>Node agent v{installed} · up to date</strong>
          <span>{hubOnly ? `Hub release v${latestRelease} did not change the node agent, so nothing needs updating.` : 'This node runs the newest agent.'}</span>
        {/if}
      </div>
      {#if outdated && canUpdate}
        <div class="actions"><button class="button primary small" type="button" disabled={busy} onclick={onUpdate}><Icon name="download" size={14} /><span>{busy ? 'Queuing…' : `Update to v${latestNode}`}</span></button></div>
      {/if}
    </div>
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {/if}
</section>

<style>
  .node-update { margin: 0 0 var(--gap); padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow); }
  .node-update.updating { border-color: color-mix(in srgb, var(--accent) 45%, var(--line)); }
  .node-update.failed { border-color: color-mix(in srgb, var(--warning) 55%, var(--line)); }
  .node-update.done { border-color: color-mix(in srgb, var(--success) 50%, var(--line)); }
  .head { display: flex; align-items: center; gap: 12px; }
  .badge { display: grid; flex-shrink: 0; place-items: center; width: 32px; height: 32px; border-radius: var(--radius-md); background: var(--accent-bg); color: var(--accent); }
  .badge.ok, .done .badge { background: var(--success-bg); color: var(--success); }
  .failed .badge { background: var(--warning-bg); color: var(--warning); }
  .updating .badge :global(svg) { animation: spin 1.4s linear infinite; }
  .title { display: grid; flex: 1; gap: 2px; min-width: 0; }
  .title strong { font-size: 13.5px; }
  .title span { color: var(--muted); font-size: 12.5px; line-height: 1.45; }
  .actions { display: flex; flex-shrink: 0; gap: 8px; }
  .steps { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 8px; margin: 14px 0 0; padding: 0; list-style: none; }
  .steps li { display: flex; align-items: center; gap: 8px; padding-top: 10px; border-top: 3px solid var(--line); color: var(--muted); font-size: 12px; }
  .steps li.complete { border-top-color: var(--success); color: var(--ink); }
  .steps li.current { border-top-color: var(--accent); color: var(--ink); font-weight: 600; }
  .dot { display: grid; place-items: center; width: 16px; height: 16px; border-radius: 50%; color: var(--success); }
  .spinner { width: 12px; height: 12px; border: 2px solid var(--accent); border-right-color: transparent; border-radius: 50%; animation: spin 0.9s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (max-width: 640px) {
    .head { flex-wrap: wrap; }
    .steps { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  }
  @media (prefers-reduced-motion: reduce) { .updating .badge :global(svg), .spinner { animation: none; } }
</style>
