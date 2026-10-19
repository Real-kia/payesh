<script lang="ts" module>
  export type InstallRoute = 'direct' | 'proxy' | 'relay';
  export type NodeRoute = 'auto' | 'proxy' | 'direct';
</script>

<script lang="ts">
  import Icon from '../Icon.svelte';
  import { preferredRelay, type RelayOption } from '../lib/relays';

  // How the hub reaches the server it installs. A proxy or relay node is for
  // servers the hub cannot reach directly (for example behind national
  // filtering). Rendered inside a .form-grid; the parent owns the values.
  let {
    route = $bindable(),
    proxyUrl = $bindable(),
    nodeRoute = $bindable(),
    relayServerId = $bindable(),
    relays
  }: {
    route: InstallRoute;
    proxyUrl: string;
    nodeRoute: NodeRoute;
    relayServerId: string;
    relays: RelayOption[];
  } = $props();

  function choose(next: InstallRoute) {
    if (route === next) return;
    route = next;
    proxyUrl = '';
    // A relay that is already set up is ready to use: preselect it.
    relayServerId = next === 'relay' ? preferredRelay(relays) : '';
  }

  let readyRelays = $derived(relays.filter((relay) => relay.ready));
  let otherRelays = $derived(relays.filter((relay) => !relay.ready));
  const label = (relay: RelayOption) => `${relay.name} · ${relay.address}${relay.countryCode ? ` · ${relay.countryCode}` : ''}`;
</script>

<fieldset class="route form-wide">
  <legend>Connection route</legend>
  <div class="segmented">
    <button type="button" aria-pressed={route === 'direct'} onclick={() => choose('direct')}><Icon name="globe" size={13} />Direct</button>
    <button type="button" aria-pressed={route === 'proxy'} onclick={() => choose('proxy')}><Icon name="shield" size={13} />Proxy</button>
    <button type="button" aria-pressed={route === 'relay'} onclick={() => choose('relay')}><Icon name="server" size={13} />Relay node</button>
  </div>
</fieldset>
{#if route === 'proxy'}
  <label class="form-wide">Proxy URL<input bind:value={proxyUrl} type="password" autocomplete="off" placeholder="socks5://user:pass@host:1080" required /></label>
{:else if route === 'relay'}
  <label class="form-wide">Relay node
    <select bind:value={relayServerId} required>
      <option value="" disabled>Choose a connected node</option>
      {#if readyRelays.length}
        <optgroup label="Ready relays">
          {#each readyRelays as relay (relay.id)}<option value={relay.id}>{label(relay)}{relay.nodes ? ` · ${relay.nodes} node${relay.nodes === 1 ? '' : 's'}` : ''}</option>{/each}
        </optgroup>
      {/if}
      {#if otherRelays.length}
        <optgroup label={readyRelays.length ? 'Other nodes' : 'Nodes'}>
          {#each otherRelays as relay (relay.id)}<option value={relay.id}>{label(relay)}</option>{/each}
        </optgroup>
      {/if}
    </select>
  </label>
{/if}
{#if route !== 'direct'}
  <label class="form-wide">Node connection
    <select bind:value={nodeRoute}>
      <option value="auto">Auto</option>
      <option value="proxy">Always via {route === 'relay' ? 'relay' : 'proxy'}</option>
      <option value="direct">Always direct</option>
    </select>
  </label>
{/if}

<style>
  .route { min-width: 0; margin: 0; padding: 0; border: 0; }
  .route legend { margin-bottom: 8px; font-size: 13px; font-weight: 550; }
</style>
