<script lang="ts">
  let {
    value = null,
    size = 110,
    strokeWidth = 9,
    label = 'Used',
    sublabel = '',
    color = '',
    showValue = true
  }: {
    value?: number | null;
    size?: number;
    strokeWidth?: number;
    label?: string;
    sublabel?: string;
    color?: string;
    showValue?: boolean;
  } = $props();

  const radius = $derived((100 - strokeWidth) / 2);
  const circumference = $derived(2 * Math.PI * radius);

  const pct = $derived(value === null || !Number.isFinite(value) ? 0 : Math.max(0, Math.min(100, value)));
  const offset = $derived(circumference * (1 - pct / 100));
  const tone = $derived(
    pct >= 90 ? 'danger' : pct >= 75 ? 'warning' : 'normal'
  );
</script>

<div class="circle-chart-container" style:--chart-size="{size}px" style:--chart-color={color || undefined}>
  <div class="circle-svg-wrap">
    <svg
      viewBox="0 0 100 100"
      class={`circle-svg ${tone}`}
      width={size}
      height={size}
      role="img"
      aria-label="{label ? `${label}: ` : ''}{value !== null && Number.isFinite(value) ? `${Math.round(pct)}%` : 'Unavailable'}"
    >
      <circle
        class="circle-bg"
        cx="50"
        cy="50"
        r={radius}
        stroke-width={strokeWidth}
      />
      {#if value !== null && Number.isFinite(value)}
        <circle
          class="circle-fill"
          cx="50"
          cy="50"
          r={radius}
          stroke-width={strokeWidth}
          stroke-dasharray={circumference}
          stroke-dashoffset={offset}
          stroke-linecap="round"
          transform="rotate(-90 50 50)"
        />
      {/if}
    </svg>
    {#if showValue}
      <div class="circle-content">
        <strong class="circle-val tabular">{value !== null && Number.isFinite(value) ? `${Math.round(value)}%` : '—'}</strong>
        {#if label && size >= 55}
          <span class="circle-lbl">{label}</span>
        {/if}
        {#if sublabel && size >= 80}
          <small class="circle-sub">{sublabel}</small>
        {/if}
      </div>
    {/if}
  </div>
</div>

<style>
  .circle-chart-container {
    display: inline-flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
  }
  .circle-svg-wrap {
    position: relative;
    width: var(--chart-size, 110px);
    height: var(--chart-size, 110px);
  }
  .circle-svg {
    width: 100%;
    height: 100%;
    overflow: visible;
  }
  .circle-bg {
    fill: none;
    stroke: var(--line);
  }
  .circle-fill {
    fill: none;
    transition: stroke-dashoffset 0.4s ease;
  }
  .circle-svg.normal .circle-fill {
    stroke: var(--chart-color, var(--teal));
  }
  .circle-svg.warning .circle-fill {
    stroke: var(--warning, #d97706);
  }
  .circle-svg.danger .circle-fill {
    stroke: var(--danger, #ef4444);
  }
  .circle-content {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    pointer-events: none;
  }
  .circle-val {
    font-size: clamp(0.65rem, calc(var(--chart-size, 110px) * 0.22), 1.25rem);
    font-weight: 700;
    letter-spacing: -0.02em;
    color: var(--ink);
    line-height: 1.1;
  }
  .circle-lbl {
    font-size: clamp(0.5rem, calc(var(--chart-size, 110px) * 0.09), 0.7rem);
    font-weight: 600;
    color: var(--muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    margin-top: 2px;
  }
  .circle-sub {
    font-size: 0.65rem;
    color: var(--muted);
  }
</style>
