// Appearance preferences are per browser (localStorage) and applied as
// attributes on <html>; public/theme-init.js reads the same keys before the
// bundle loads so the first paint already matches.

export type ThemeMode = 'light' | 'dark' | 'system';
export type Accent = 'blue' | 'teal' | 'violet' | 'amber';
export type Motion = 'full' | 'reduced';
export type Density = 'comfortable' | 'compact';

export type Appearance = { mode: ThemeMode; accent: Accent; motion: Motion; density: Density };

export const ACCENTS: { id: Accent; label: string; swatch: string }[] = [
  { id: 'blue', label: 'Blue', swatch: '#2f6bd8' },
  { id: 'teal', label: 'Teal', swatch: '#0d8a7f' },
  { id: 'violet', label: 'Violet', swatch: '#6d4fd6' },
  { id: 'amber', label: 'Amber', swatch: '#b25c08' }
];

function read(key: string): string | null {
  try { return window.localStorage.getItem(key); } catch { return null; }
}

function write(key: string, value: string | null): void {
  try {
    if (value === null) window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, value);
  } catch { /* storage can be unavailable (private mode); preferences then last for this tab */ }
}

export function loadAppearance(): Appearance {
  if (typeof window === 'undefined') return { mode: 'system', accent: 'blue', motion: 'full', density: 'comfortable' };
  const mode = read('payesh-theme');
  const accent = read('payesh-accent');
  return {
    mode: mode === 'light' || mode === 'dark' ? mode : 'system',
    accent: accent === 'teal' || accent === 'violet' || accent === 'amber' ? accent : 'blue',
    motion: read('payesh-motion') === 'reduced' ? 'reduced' : 'full',
    density: read('payesh-density') === 'compact' ? 'compact' : 'comfortable'
  };
}

export function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia?.('(prefers-color-scheme: dark)').matches;
}

export function resolveTheme(mode: ThemeMode): 'light' | 'dark' {
  return mode === 'system' ? (systemPrefersDark() ? 'dark' : 'light') : mode;
}

let transitionTimer: number | undefined;

/** Applies appearance to <html>. With animate, colours cross-fade once. */
export function applyAppearance(appearance: Appearance, animate = false): void {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  if (animate && appearance.motion === 'full' && !window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) {
    root.classList.add('theme-transition');
    window.clearTimeout(transitionTimer);
    transitionTimer = window.setTimeout(() => root.classList.remove('theme-transition'), 420);
  }
  root.dataset.theme = resolveTheme(appearance.mode);
  if (appearance.accent === 'blue') delete root.dataset.accent; else root.dataset.accent = appearance.accent;
  if (appearance.motion === 'reduced') root.dataset.motion = 'reduced'; else delete root.dataset.motion;
  if (appearance.density === 'compact') root.dataset.density = 'compact'; else delete root.dataset.density;
}

export function saveAppearance(appearance: Appearance): void {
  write('payesh-theme', appearance.mode === 'system' ? null : appearance.mode);
  write('payesh-accent', appearance.accent === 'blue' ? null : appearance.accent);
  write('payesh-motion', appearance.motion === 'reduced' ? 'reduced' : null);
  write('payesh-density', appearance.density === 'compact' ? 'compact' : null);
}
