/** The fresh hub starts with HTTP on its own default dashboard port. */
export function hubDashboardURL(host: string): string {
  const address = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
  const url = new URL(`http://${address}:8787`);
  if (url.username || url.password || url.pathname !== '/' || url.search || url.hash || url.port !== '8787') throw new Error('Invalid hub address');
  return url.origin;
}
