<script lang="ts">
  import Icon from '../Icon.svelte';

  type NavPage = 'overview' | 'monitoring' | 'servers' | 'alerts' | 'logs' | 'packages' | 'settings';

  let { activePage, serverCount, firingAlerts, statusLabel, statusTone, user, inert = false, onNavigate }: {
    activePage: string;
    serverCount: number;
    firingAlerts: number;
    statusLabel: string;
    statusTone: 'ok' | 'warn' | 'off';
    user: string;
    inert?: boolean;
    onNavigate: (page: NavPage, fromMenu: boolean) => void;
  } = $props();

  let menuOpen = $state(false);

  const groups: { label: string; items: { page: NavPage; label: string; icon: string }[] }[] = [
    { label: 'Monitor', items: [
      { page: 'overview', label: 'Overview', icon: 'overview' },
      { page: 'monitoring', label: 'Server monitoring', icon: 'activity' },
      { page: 'servers', label: 'Servers', icon: 'servers' },
      { page: 'alerts', label: 'Alerts', icon: 'alerts' },
      { page: 'logs', label: 'Logs', icon: 'terminal' }
    ] },
    { label: 'Manage', items: [
      { page: 'packages', label: 'Packages', icon: 'packages' },
      { page: 'settings', label: 'Settings', icon: 'settings' }
    ] }
  ];

  function isActive(page: NavPage): boolean {
    if (page === 'servers') return ['servers', 'server', 'add-server', 'install-progress'].includes(activePage);
    return activePage === page;
  }

  function go(page: NavPage) {
    const fromMenu = menuOpen;
    menuOpen = false;
    onNavigate(page, fromMenu);
  }
</script>

<aside class="sidebar" aria-label="Primary navigation" {inert}>
  <div class="brand">
    <svg class="brand-mark" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="9" /><path d="M6 18h4.2l2.6-6.5 4.4 11 3-7.5H26" /></svg>
    <div class="brand-text"><strong>Payesh</strong><small>Fleet monitor</small></div>
    <button class="menu-toggle icon-button" type="button" aria-expanded={menuOpen} aria-controls="primary-navigation" aria-label={menuOpen ? 'Close menu' : 'Open menu'} onclick={() => (menuOpen = !menuOpen)}>
      <Icon name={menuOpen ? 'x' : 'menu'} size={18} />
    </button>
  </div>

  <nav id="primary-navigation" class="nav" class:open={menuOpen}>
    {#each groups as group (group.label)}
      <div class="group">
        <span class="group-label">{group.label}</span>
        {#each group.items as item (item.page)}
          <button class="nav-item" class:active={isActive(item.page)} type="button" aria-current={isActive(item.page) ? 'page' : undefined} onclick={() => go(item.page)}>
            <Icon name={item.icon} size={17} />
            <span>{item.label}</span>
            {#if item.page === 'servers'}<span class="count">{serverCount}</span>{/if}
            {#if item.page === 'alerts' && firingAlerts > 0}<span class="count firing" aria-label={`${firingAlerts} firing`}>{firingAlerts}</span>{/if}
          </button>
        {/each}
      </div>
    {/each}
  </nav>

  <div class="footer">
    <span class={`status ${statusTone}`}><span class="dot"></span>{statusLabel}</span>
    {#if user}<span class="user" title={`Signed in as ${user}`}><span class="avatar" aria-hidden="true">{user.slice(0, 1).toUpperCase()}</span>{user}</span>{/if}
  </div>
</aside>

<style>
  .sidebar {
    position: sticky;
    top: 0;
    display: flex;
    flex-direction: column;
    gap: 22px;
    height: 100vh;
    height: 100dvh;
    padding: 18px 12px 14px;
    overflow-y: auto;
    border-right: 1px solid var(--line);
    background: var(--surface);
  }
  .brand { display: flex; align-items: center; gap: 11px; padding: 4px 8px; }
  .brand-mark { width: 34px; height: 34px; flex-shrink: 0; filter: drop-shadow(0 4px 10px var(--accent-glow)); }
  .brand-mark rect { fill: var(--accent); }
  .brand-mark path { fill: none; stroke: var(--accent-contrast); stroke-width: 2.6; stroke-linecap: round; stroke-linejoin: round; }
  .brand-text { display: grid; line-height: 1.15; }
  .brand-text strong { font-size: 15px; font-weight: 700; letter-spacing: -0.02em; }
  .brand-text small { color: var(--muted); font-size: 11px; }
  .menu-toggle { display: none; margin-left: auto; }

  .nav { display: flex; flex-direction: column; gap: 18px; }
  .group { display: flex; flex-direction: column; gap: 2px; }
  .group-label { padding: 0 12px 6px; color: var(--muted); font-size: 10.5px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; opacity: 0.8; }
  .nav-item {
    position: relative;
    display: flex;
    align-items: center;
    gap: 11px;
    width: 100%;
    min-height: 36px;
    padding: 0 12px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--ink-secondary);
    font-size: 13.5px;
    font-weight: 500;
    text-align: left;
    transition: background-color var(--transition-fast), color var(--transition-fast);
  }
  .nav-item :global(svg) { color: var(--muted); transition: color var(--transition-fast), transform var(--dur) var(--ease-spring); }
  .nav-item:hover { background: var(--surface-muted); color: var(--ink); }
  .nav-item:hover :global(svg) { transform: scale(1.08); }
  .nav-item::before {
    content: '';
    position: absolute;
    left: -12px;
    top: 8px;
    bottom: 8px;
    width: 3px;
    border-radius: 0 3px 3px 0;
    background: var(--accent);
    transform: scaleY(0);
    transition: transform var(--dur) var(--ease-out);
  }
  .nav-item.active { background: var(--accent-bg); color: var(--ink); font-weight: 620; }
  .nav-item.active :global(svg) { color: var(--accent); }
  .nav-item.active::before { transform: scaleY(1); }
  .count { margin-left: auto; min-width: 22px; padding: 1px 7px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-weight: 600; text-align: center; font-variant-numeric: tabular-nums; }
  .nav-item.active .count:not(.firing) { background: var(--surface); }
  .count.firing { background: var(--danger); color: #fff; animation: pop-in var(--dur) var(--ease-spring); }

  .footer { display: grid; gap: 10px; margin-top: auto; padding: 12px 10px 4px; border-top: 1px solid var(--line); color: var(--muted); font-size: 12px; }
  .status { display: inline-flex; align-items: center; gap: 8px; }
  .dot { position: relative; width: 7px; height: 7px; border-radius: 50%; background: var(--muted); }
  .status.ok .dot { background: var(--success); }
  .status.ok .dot::after { content: ''; position: absolute; inset: 0; border-radius: inherit; background: inherit; animation: ping 1.2s var(--ease-out) 2; }
  .status.warn .dot { background: var(--warning); }
  .user { display: inline-flex; align-items: center; gap: 8px; overflow: hidden; color: var(--ink-secondary); text-overflow: ellipsis; white-space: nowrap; }
  .avatar { display: grid; flex-shrink: 0; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--accent-bg); color: var(--accent); font-size: 11px; font-weight: 700; }

  @media (max-width: 960px) {
    .sidebar { z-index: 60; flex-direction: column; gap: 0; height: auto; padding: 10px 16px; overflow: visible; border-right: 0; border-bottom: 1px solid var(--line); }
    .brand { padding: 0; }
    .menu-toggle { display: grid; }
    .nav { display: none; }
    .nav.open { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; padding: 14px 0 6px; animation: rise-in var(--dur) var(--ease-out); }
    .nav-item { min-height: 42px; }
    .nav-item::before { left: 0; }
    .footer { display: none; }
  }
  @media (max-width: 480px) { .nav.open { grid-template-columns: 1fr; } }
</style>
