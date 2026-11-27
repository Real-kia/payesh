<script lang="ts">
  import FleetTrafficHistory from '../components/FleetTrafficHistory.svelte';
  import LiveIndicator from '../components/LiveIndicator.svelte';
  import { PREVIEW_MODE } from '../lib/env';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, theme, refreshing, updatedAt, onAuthExpired, onOpenStorage }: {
    servers: PreviewServer[];
    timezone: string;
    theme: string;
    refreshing: boolean;
    updatedAt: number | null;
    onAuthExpired: () => void;
    onOpenStorage?: () => void;
  } = $props();
</script>

<section class="page" aria-labelledby="fleet-traffic-title">
  <div class="page-heading">
    <div>
      <h1 id="fleet-traffic-title" tabindex="-1">Fleet traffic</h1>
      <p class="lede">Compare live network rates with recorded traffic across your fleet or by server tag.</p>
    </div>
    {#if !PREVIEW_MODE}<LiveIndicator {refreshing} {updatedAt} />{/if}
  </div>
  <FleetTrafficHistory {servers} {timezone} {theme} {onAuthExpired} {onOpenStorage} />
</section>
