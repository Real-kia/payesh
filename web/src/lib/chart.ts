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

/** A vertical gradient fill that fades a series colour out towards the x axis. */
export function gradientFill(color: string): uPlot.Series.Fill {
  return (u: uPlot) => {
    const { top, height } = u.bbox;
    if (!Number.isFinite(top) || !Number.isFinite(height) || height <= 0) return withAlpha(color, 0.1);
    const gradient = u.ctx.createLinearGradient(0, top, 0, top + height);
    gradient.addColorStop(0, withAlpha(color, 0.26));
    gradient.addColorStop(1, withAlpha(color, 0.01));
    return gradient;
  };
}
