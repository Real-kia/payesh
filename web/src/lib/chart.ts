import type uPlot from 'uplot';

/** Reads the current theme colours that charts need from CSS variables. */
export function chartColors(element: Element): { muted: string; line: string; surface: string; read: (name: string, fallback: string) => string } {
  const styles = getComputedStyle(element);
  const read = (name: string, fallback: string) => styles.getPropertyValue(name).trim() || fallback;
  return { muted: read('--muted', '#8a98aa'), line: read('--line-light', 'rgba(127,127,127,0.15)'), surface: read('--surface', '#ffffff'), read };
}

/** Converts #rgb, #rrggbb or rgb()/rgba() to rgba() with the given alpha. */
export function withAlpha(color: string, alpha: number): string {
  const hex = color.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i);
  if (hex) {
    const value = hex[1].length === 3 ? hex[1].split('').map((c) => c + c).join('') : hex[1];
    const n = Number.parseInt(value, 16);
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
  }
  const rgb = color.match(/^rgba?\(([^)]+)\)$/i);
  if (rgb) {
    const [r, g, b] = rgb[1].split(/[\s,/]+/).filter(Boolean);
    return `rgba(${r}, ${g}, ${b}, ${alpha})`;
  }
  return color;
}

/** A flat, faint area under a series: enough to read the shape, not a decoration. */
export function areaFill(color: string): uPlot.Series.Fill {
  return withAlpha(color, 0.07);
}

/** Displays the hovered timestamp and values beside the cursor. */
export function hoverValues(timezone: string): uPlot.Plugin {
  let tooltip: HTMLDivElement;
  return { hooks: {
    ready: [(plot) => {
      tooltip = document.createElement('div');
      tooltip.setAttribute('role', 'status');
      Object.assign(tooltip.style, {
        position: 'absolute', display: 'none', pointerEvents: 'none', zIndex: '10',
        padding: '8px 10px', borderRadius: '6px', border: '1px solid var(--line)',
        background: 'var(--surface)', color: 'var(--ink)', font: '12px system-ui',
        whiteSpace: 'pre', boxShadow: '0 2px 8px #0002'
      });
      plot.over.append(tooltip);
    }],
    setCursor: [(plot) => {
      const { idx, left, top } = plot.cursor;
      if (!tooltip || idx == null || left == null || top == null || left < 0 || top < 0) {
        if (tooltip) tooltip.style.display = 'none';
        return;
      }
      const timestamp = Number(plot.data[0][idx]);
      const lines = [new Date(timestamp * 1000).toLocaleString([], { timeZone: timezone })];
      plot.series.slice(1).forEach((series, index) => {
        if (series.show === false) return;
        const raw = plot.data[index + 1][idx];
        const value = typeof series.value === 'function' ? series.value(plot, raw as number, index + 1, idx) : String(raw ?? '—');
        lines.push(`${series.label}: ${value}`);
      });
      tooltip.textContent = lines.join('\n');
      tooltip.style.display = 'block';
      tooltip.style.left = `${Math.max(0, Math.min(left + 12, plot.over.clientWidth - tooltip.offsetWidth))}px`;
      tooltip.style.top = `${Math.max(0, Math.min(top + 12, plot.over.clientHeight - tooltip.offsetHeight))}px`;
    }],
    destroy: [() => tooltip?.remove()]
  } };
}
