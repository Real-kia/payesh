<script lang="ts">
  let { rows = 4, cards = 3, label = 'Loading' }: { rows?: number; cards?: number; label?: string } = $props();
</script>

<div class="skeleton-page" role="status" aria-live="polite" aria-label={label}>
  {#if cards > 0}
    <div class="cards">
      {#each Array(cards) as _, index (index)}
        <div class="card"><span class="skeleton line short"></span><span class="skeleton value"></span><span class="skeleton line"></span></div>
      {/each}
    </div>
  {/if}
  {#if rows > 0}
    <div class="table">
      {#each Array(rows) as _, index (index)}
        <div class="row" style:opacity={1 - index * 0.14}>
          <span class="skeleton pill"></span>
          <span class="name"><span class="skeleton line"></span><span class="skeleton line short"></span></span>
          <span class="skeleton line"></span>
          <span class="skeleton line"></span>
        </div>
      {/each}
    </div>
  {/if}
  <span class="sr-only">{label}…</span>
</div>

<style>
  .skeleton-page { display: grid; gap: 24px; animation: fade-in var(--dur-slow) var(--ease-out); }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: var(--gap); }
  .card { display: grid; gap: 12px; padding: 18px 20px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); }
  .table { overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); }
  .row { display: grid; grid-template-columns: 90px minmax(0, 2fr) 1fr 1fr; gap: 20px; align-items: center; padding: 18px 20px; border-bottom: 1px solid var(--line-light); }
  .row:last-child { border-bottom: 0; }
  .name { display: grid; gap: 8px; }
  .line { height: 10px; }
  .line.short { width: 55%; }
  .value { width: 40%; height: 26px; }
  .pill { width: 72px; height: 20px; border-radius: var(--radius-pill); }
  @media (max-width: 640px) { .row { grid-template-columns: 70px 1fr; } .row > :nth-child(n + 3) { display: none; } }
</style>
