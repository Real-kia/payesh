<script lang="ts">
  import Icon from '../Icon.svelte';

  // SSH connection fields shared by "Add a server" and the install retry on a
  // server page. Rendered inside a .form-grid; the parent owns the values.
  let {
    host = $bindable(),
    port = $bindable(),
    user = $bindable(),
    method = $bindable(),
    password = $bindable(),
    key = $bindable(),
    passphrase = $bindable(),
    fingerprint = $bindable(),
    hostLabel = 'IP address or hostname',
    showHint = true
  }: {
    host: string;
    port: string;
    user: string;
    method: 'password' | 'key';
    password: string;
    key: string;
    passphrase: string;
    fingerprint: string;
    hostLabel?: string;
    showHint?: boolean;
  } = $props();

  // Switching method clears both secrets so only one is ever sent.
  function choose(next: 'password' | 'key') {
    if (method === next) return;
    method = next;
    password = '';
    key = '';
    passphrase = '';
  }
</script>

<label class="form-wide">{hostLabel}<input bind:value={host} maxlength="255" placeholder="e.g. 192.0.2.1" required autocomplete="off" /></label>
<label>SSH port<input type="number" min="1" max="65535" bind:value={port} required /></label>
<label>SSH user<input bind:value={user} placeholder="root" required autocomplete="off" /></label>
<fieldset class="auth form-wide">
  <legend>SSH authentication</legend>
  <div class="segmented">
    <button type="button" aria-pressed={method === 'password'} onclick={() => choose('password')}><Icon name="lock" size={13} />Password</button>
    <button type="button" aria-pressed={method === 'key'} onclick={() => choose('key')}><Icon name="shield" size={13} />Private key</button>
  </div>
  {#if showHint}<small>Credentials are used for this installation only and are not stored in the browser.</small>{/if}
</fieldset>
{#if method === 'password'}
  <label class="form-wide">SSH password<input type="password" bind:value={password} autocomplete="off" placeholder="SSH user password" required /></label>
{:else}
  <label class="form-wide">OpenSSH private key<textarea bind:value={key} rows="4" autocomplete="off" placeholder="Paste OpenSSH private key" required></textarea></label>
  <label class="form-wide"><span>Key passphrase <small>(if encrypted)</small></span><input type="password" bind:value={passphrase} autocomplete="off" /></label>
{/if}
<label class="form-wide"><span>Host-key fingerprint <small>(required for new hosts)</small></span><input bind:value={fingerprint} placeholder="SHA256:…" autocomplete="off" /><small>Verify the SHA256 fingerprint through a trusted channel before installing. A matching existing known_hosts entry can be reused.</small></label>

<style>
  .auth { min-width: 0; margin: 0; padding: 0; border: 0; }
  .auth legend { margin-bottom: 8px; font-size: 13px; font-weight: 550; }
  .auth small { display: block; margin-top: 8px; color: var(--muted); font-size: 11.5px; }
</style>
