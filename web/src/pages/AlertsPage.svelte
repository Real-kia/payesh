<script lang="ts">
  import { serverLabel } from '../lib/flags';
  import Icon from '../Icon.svelte';
  import StatusPill from '../components/StatusPill.svelte';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import { formatDateTimeInTz } from '../timezone';
  import { alertCategories, categoryForAlert, categoryForRule, type AlertCategory } from '../lib/alertCategories';
  import type { AlertRule, AlertState } from '../api';
  import type { PreviewServer } from '../preview/fixtures';

  let { alerts, rules, loadState, error, servers, timezone, onReload, canEdit, onCreateRule, onSetRuleEnabled }: {
    alerts: AlertState[];
    rules: AlertRule[];
    loadState: 'ready' | 'loading' | 'empty' | 'error';
    error: string;
    servers: PreviewServer[];
    timezone: string;
    onReload: () => void;
    canEdit: boolean;
    onCreateRule: (input: { name: string; expression: string; duration_seconds?: number }) => Promise<AlertRule>;
    onSetRuleEnabled: (rule: AlertRule, enabled: boolean) => Promise<void>;
  } = $props();

  let page = $state(0);
  let pageSize = $state(25);
  let stateFilter = $state<'all' | 'firing' | 'pending' | 'recovered'>('all');
  let categoryFilter = $state<'all' | AlertCategory>('all');
  let showRuleForm = $state(false);
  let newRuleName = $state('');
  let newRuleMetric = $state('cpu.utilization');
  let newRuleThreshold = $state(85);
  let ruleBusy = $state(false);
  let ruleError = $state('');
  const metricLabels: Record<string, string> = { 'cpu.utilization': 'CPU utilization', 'memory.used_percent': 'Memory utilization', 'disk.root.used_percent': 'Disk utilization', 'memory.swap_used_percent': 'Swap utilization', 'node.unreachable': 'Node unreachable' };
  function expressionForRule(): string { return newRuleMetric === 'node.unreachable' ? 'node.unreachable == 1' : `${newRuleMetric}>${Number(newRuleThreshold)}`; }
  async function addRule() {
    if (!newRuleName.trim() || !Number.isFinite(Number(newRuleThreshold)) || Number(newRuleThreshold) < 1 || ruleBusy) return;
    ruleBusy = true; ruleError = '';
    try { await onCreateRule({ name: newRuleName.trim(), expression: expressionForRule() }); newRuleName = ''; showRuleForm = false; }
    catch (error) { ruleError = error instanceof Error ? error.message : 'Unable to create this alert rule.'; }
    finally { ruleBusy = false; }
  }

  async function toggleRule(rule: AlertRule) {
    if (ruleBusy) return;
    ruleBusy = true; ruleError = '';
    try { await onSetRuleEnabled(rule, !rule.enabled); }
    catch (error) { ruleError = error instanceof Error ? error.message : 'Unable to update this rule.'; }
    finally { ruleBusy = false; }
  }

  const alertCategory = $derived(new Map(alerts.map((alert) => [alert.id, categoryForAlert(alert, rules)])));
  const inCategory = $derived(alerts.filter((alert) => categoryFilter === 'all' || alertCategory.get(alert.id) === categoryFilter));
  const categoryCounts = $derived(alertCategories
    .map((category) => {
      const items = alerts.filter((alert) => alertCategory.get(alert.id) === category.id);
      return { ...category, total: items.length, firing: items.filter((alert) => alert.state === 'firing').length };
    })
    .filter((category) => category.total > 0));
  const ruleGroups = $derived(alertCategories
    .map((category) => ({ ...category, rules: rules.filter((rule) => categoryForRule(rule) === category.id) }))
    .filter((group) => group.rules.length > 0));
  const filtered = $derived(inCategory.filter((alert) => stateFilter === 'all' || (stateFilter === 'recovered' ? alert.state !== 'firing' && alert.state !== 'pending' : alert.state === stateFilter)));
  const pageCount = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
  const paged = $derived(filtered.slice(page * pageSize, (page + 1) * pageSize));
  const counts = $derived({
    firing: inCategory.filter((alert) => alert.state === 'firing').length,
    pending: inCategory.filter((alert) => alert.state === 'pending').length
  });
  $effect(() => { if (categoryFilter !== 'all' && !categoryCounts.some((category) => category.id === categoryFilter)) categoryFilter = 'all'; });
  function categoryMeta(id: AlertCategory | undefined) {
    return alertCategories.find((category) => category.id === id) ?? alertCategories[alertCategories.length - 1];
  }
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });

  function serverName(id?: string): string {
    const server = servers.find((item) => item.id === id);
    return server ? serverLabel(server) : id || 'Fleet-wide';
  }
  function ruleLabel(rule: string): string {
    return rule.replace(/[-_]+/g, ' ').replace(/^\w/, (c) => c.toUpperCase());
  }
  function tone(state: string): string {
    return state === 'firing' ? 'failed' : state === 'pending' ? 'stale' : 'healthy';
  }
</script>

<section class="page" aria-labelledby="alerts-title">
  <div class="page-heading">
    <div>
      <h1 id="alerts-title" tabindex="-1">Alerts</h1>
      <p class="lede">Rule states evaluated by the hub.</p>
    </div>
    <button class="button ghost" type="button" onclick={onReload} disabled={loadState === 'loading'}><Icon name="refresh" size={14} /><span>Reload</span></button>
  </div>

  <article class="panel rules-panel">
    <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="bell" size={16} /></span><div><h2>Alert rules</h2><span class="muted">Rules watch telemetry and create an alert event when a condition stays true.</span></div></div>{#if canEdit}<button class="button primary small" type="button" onclick={() => (showRuleForm = !showRuleForm)}><Icon name="plus" size={13} />Add rule</button>{/if}</div>
    {#if showRuleForm}<form class="rule-form" onsubmit={(event) => { event.preventDefault(); void addRule(); }}><input placeholder="Rule name" aria-label="Rule name" bind:value={newRuleName} /><select aria-label="Metric" bind:value={newRuleMetric}><option value="cpu.utilization">CPU utilization</option><option value="memory.used_percent">Memory utilization</option><option value="disk.root.used_percent">Disk utilization</option><option value="memory.swap_used_percent">Swap utilization</option><option value="node.unreachable">Node unreachable (0 = reachable, 1 = unreachable)</option></select><input type="number" min="1" aria-label="Threshold" bind:value={newRuleThreshold} /><button class="button primary small" type="submit" disabled={ruleBusy}>{ruleBusy ? 'Creating…' : 'Create rule'}</button></form>{/if}
    {#if ruleError}<p class="form-error" role="alert">{ruleError}</p>{/if}
    {#each ruleGroups as group (group.id)}
      <div class="rule-group">
        <h3 class="rule-group-title"><Icon name={group.icon} size={13} />{group.label}<span class="n">{group.rules.length}</span></h3>
        <div class="rule-list">{#each group.rules as rule (rule.id)}<div class="rule-row"><span class:disabled={!rule.enabled} class="rule-state"></span><div><strong>{rule.name}</strong><small>{rule.expression} · for {rule.duration_seconds}s</small></div>{#if canEdit}<button class="button ghost small" type="button" onclick={() => void toggleRule(rule)}>{rule.enabled ? 'Disable' : 'Enable'}</button>{/if}</div>{/each}</div>
      </div>
    {:else}<p class="muted">No alert rules have been configured.</p>{/each}
  </article>

  {#if loadState === 'loading'}
    <LoadingSkeleton cards={0} rows={4} label="Loading alert states" />
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={24} /></div>
      <h2>Could not load alerts</h2>
      <p>{error}</p>
      <button class="button primary" type="button" onclick={onReload}>Retry</button>
    </div>
  {:else if loadState === 'empty'}
    <div class="state-panel">
      <div class="state-icon positive"><Icon name="check" size={24} /></div>
      <h2>All systems operating normally</h2>
      <p>No active incidents or firing alert rules across the fleet.</p>
    </div>
  {:else}
    <div class="category-bar" role="group" aria-label="Filter alerts by category">
      <button type="button" class="category-chip" aria-pressed={categoryFilter === 'all'} onclick={() => { categoryFilter = 'all'; page = 0; }}>All categories <span class="n">{alerts.length}</span></button>
      {#each categoryCounts as category (category.id)}
        <button type="button" class="category-chip" aria-pressed={categoryFilter === category.id} onclick={() => { categoryFilter = category.id; page = 0; }}>
          <Icon name={category.icon} size={13} />{category.label}
          <span class="n">{category.total}</span>
          {#if category.firing > 0}<span class="firing-count" title={`${category.firing} firing`}>{category.firing}</span>{/if}
        </button>
      {/each}
    </div>

    <div class="toolbar">
      <div class="segmented" role="group" aria-label="Filter alerts by state">
        <button type="button" aria-pressed={stateFilter === 'all'} onclick={() => { stateFilter = 'all'; page = 0; }}>All <span class="n">{inCategory.length}</span></button>
        <button type="button" aria-pressed={stateFilter === 'firing'} onclick={() => { stateFilter = 'firing'; page = 0; }}><i class="dot firing"></i>Firing <span class="n">{counts.firing}</span></button>
        <button type="button" aria-pressed={stateFilter === 'pending'} onclick={() => { stateFilter = 'pending'; page = 0; }}><i class="dot pending"></i>Pending <span class="n">{counts.pending}</span></button>
        <button type="button" aria-pressed={stateFilter === 'recovered'} onclick={() => { stateFilter = 'recovered'; page = 0; }}>Recovered <span class="n">{inCategory.length - counts.firing - counts.pending}</span></button>
      </div>
      <label class="page-size">Per page
        <select bind:value={pageSize} onchange={() => (page = 0)}>
          <option value={10}>10</option><option value={25}>25</option><option value={50}>50</option><option value={100}>100</option>
        </select>
      </label>
    </div>

    {#if paged.length === 0}
      <div class="unavailable-panel"><strong>No alerts match these filters</strong><span>Choose another filter.</span></div>
    {:else}
      <div class="alert-list stagger">
        {#each paged as alert, index (alert.id)}
          <article class={`alert-row ${alert.state}`} style:--i={Math.min(index, 10)}>
            <StatusPill state={tone(alert.state)} label={alert.state} />
            <div class="what">
              <strong>{rules.find((rule) => rule.id === alert.rule_id)?.name ?? ruleLabel(alert.rule_id)}</strong>
              <small><span class="category-tag"><Icon name={categoryMeta(alertCategory.get(alert.id)).icon} size={11} />{categoryMeta(alertCategory.get(alert.id)).label}</span><Icon name="server" size={11} /> {serverName(alert.server_id)}</small>
            </div>
            <span class="when"><Icon name="clock" size={12} /> {alert.last_observation ? formatDateTimeInTz(alert.last_observation, timezone) : 'Awaiting observation'}</span>
            {#if alert.last_value !== undefined}
              <span class="value"><strong class="tabular">{alert.last_value.toFixed(2)}</strong><small>last value</small></span>
            {:else}<span></span>{/if}
          </article>
        {/each}
      </div>
    {/if}

    <div class="pager">
      <span class="muted">Showing {filtered.length === 0 ? 0 : page * pageSize + 1}–{Math.min(filtered.length, (page + 1) * pageSize)} of {filtered.length}</span>
      {#if pageCount > 1}
        <div class="pager-buttons">
          <button class="button ghost small" type="button" disabled={page === 0} onclick={() => page--}>Previous</button>
          <span class="muted tabular">Page {page + 1} of {pageCount}</span>
          <button class="button ghost small" type="button" disabled={page >= pageCount - 1} onclick={() => page++}>Next</button>
        </div>
      {/if}
    </div>
  {/if}
</section>

<style>
  .toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 14px; }
  .rules-panel { margin-bottom: 16px; }
  .rule-group + .rule-group { margin-top: 14px; }
  .rule-group-title { display: flex; align-items: center; gap: 6px; margin: 0 0 8px; color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase; }
  .category-bar { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; }
  .category-chip { display: inline-flex; align-items: center; gap: 6px; padding: 6px 12px; border: 1px solid var(--line); border-radius: var(--radius-pill); background: var(--surface); color: var(--ink); font-size: 12.5px; cursor: pointer; }
  .category-chip:hover { background: var(--surface-hover); }
  .category-chip[aria-pressed='true'] { border-color: var(--accent); background: var(--accent-bg); }
  .firing-count { padding: 0 6px; border-radius: var(--radius-pill); background: var(--danger); color: #fff; font-size: 11px; font-variant-numeric: tabular-nums; }
  .category-tag { display: inline-flex; align-items: center; gap: 4px; margin-right: 6px; padding: 0 6px; border-radius: var(--radius-pill); background: var(--surface-muted); }
  .rule-list { display: grid; gap: 8px; }
  .rule-row { display: flex; align-items: center; gap: 10px; padding: 10px 12px; border: 1px solid var(--line-light); border-radius: var(--radius-md); background: var(--surface-muted); }
  .rule-row > div { display: grid; flex: 1; gap: 2px; }
  .rule-row small { color: var(--muted); font-size: 12px; }
  .rule-state { width: 8px; height: 8px; border-radius: 50%; background: var(--success); }
  .rule-state.disabled { background: var(--muted); }
  .rule-form { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 10px; padding: 10px; border: 1px solid var(--line-light); border-radius: var(--radius-md); }
  .rule-form input, .rule-form select { min-width: 140px; }
  .n { padding: 0 6px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .dot { width: 6px; height: 6px; border-radius: 50%; }
  .dot.firing { background: var(--danger); }
  .dot.pending { background: var(--warning); }
  .page-size { display: inline-flex; align-items: center; gap: 8px; color: var(--muted); font-size: 12px; }
  .page-size select { padding-top: 5px; padding-bottom: 5px; }

  .alert-list { overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow); }
  .alert-row {
    position: relative;
    display: grid;
    grid-template-columns: 110px minmax(0, 1.5fr) minmax(0, 1fr) 90px;
    gap: 16px;
    align-items: center;
    padding: var(--row-pad-y) 20px;
    border-bottom: 1px solid var(--line-light);
  }
  .alert-row:last-child { border-bottom: 0; }
  .alert-row.firing { background: linear-gradient(90deg, var(--danger-bg), transparent 40%); }
  .alert-row.firing::before { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 3px; background: var(--danger); }
  .what { min-width: 0; }
  .what strong { display: block; overflow: hidden; font-size: 13.5px; text-overflow: ellipsis; white-space: nowrap; }
  .what small, .when { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); font-size: 12px; }
  .value { display: grid; justify-items: end; }
  .value small { color: var(--muted); font-size: 11px; }

  .pager { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-top: 14px; font-size: 12.5px; }
  .pager-buttons { display: flex; align-items: center; gap: 10px; }

  @media (max-width: 760px) {
    .alert-row { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'pill value' 'what what' 'when when'; gap: 8px 12px; }
    .alert-row > :global(:first-child) { grid-area: pill; justify-self: start; }
    .what { grid-area: what; }
    .what small { flex-wrap: wrap; }
    .when { grid-area: when; }
    .value { grid-area: value; }
  }
</style>
