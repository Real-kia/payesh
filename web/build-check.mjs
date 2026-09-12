import { readFileSync } from 'node:fs';

const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url)));
const lock = JSON.parse(readFileSync(new URL('./package-lock.json', import.meta.url)));
if (pkg.dependencies.svelte !== '5.38.7' || pkg.dependencies.uplot !== '1.6.32' || pkg.devDependencies['@sveltejs/vite-plugin-svelte'] !== '6.2.1' || pkg.devDependencies.typescript !== '5.9.2' || pkg.devDependencies.vite !== '7.1.5' || pkg.devDependencies['svelte-check'] !== '4.3.1') {
  throw new Error('frontend dependency pins changed without contract review');
}
const root = lock.packages?.[''];
const samePins = (a = {}, b = {}) => Object.keys(a).length === Object.keys(b).length && Object.entries(a).every(([name, version]) => b[name] === version);
if (!root || !samePins(root.dependencies, pkg.dependencies) || !samePins(root.devDependencies, pkg.devDependencies)) {
  throw new Error('package-lock.json is out of sync with reviewed pins');
}
const app = readFileSync(new URL('./src/App.svelte', import.meta.url), 'utf8');
const fixtures = readFileSync(new URL('./src/preview/fixtures.ts', import.meta.url), 'utf8');
const chart = readFileSync(new URL('./src/ChartPreview.svelte', import.meta.url), 'utf8');
const api = readFileSync(new URL('./src/api.ts', import.meta.url), 'utf8');
for (const marker of ['PREVIEW_MODE', 'localStorage', 'prefers-color-scheme', 'ChartPreview', 'setupSecret', 'pairingToken', 'loadPreviewData', 'hasCapability', 'metricHistory', 'aria-label']) {
  if (!app.includes(marker)) throw new Error(`preview UI contract missing ${marker}`);
}
for (const state of ['healthy', 'stale', 'pending', 'unreachable', 'installing', 'failed', 'disabled', 'unsupported']) {
  if (!fixtures.includes(`displayState: '${state}'`)) throw new Error(`preview fixture missing ${state} state`);
}
if (!app.includes('pairingToken.length < 16') || !app.includes('minlength="16"')) throw new Error('pairing-token contract is not enforced at 16 characters');
if (chart.includes('const samples')) throw new Error('chart fixture samples must remain behind the preview adapter');
for (const marker of ['credentials: \'include\'', 'listServers(', 'getServer(', 'queryMetrics(', 'queryTraffic(', 'queryLogs(', 'AbortSignal', 'authExpired', 'mapWithConcurrency', 'concurrency must be a positive integer']) {
  if (!api.includes(marker) && !app.includes(marker)) throw new Error(`API adapter contract missing ${marker}`);
}
console.log('frontend foundation pins and preview contract valid');
