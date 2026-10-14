/**
 * Turns an ISO 3166-1 alpha-2 code into its flag emoji. Anything that is not
 * exactly two letters yields '' so a failed or missing lookup shows no flag.
 */
export function countryFlag(code: string | null | undefined): string {
  if (typeof code !== 'string' || !/^[A-Za-z]{2}$/.test(code)) return '';
  const upper = code.toUpperCase();
  return String.fromCodePoint(...[...upper].map((letter) => 0x1f1e6 + letter.charCodeAt(0) - 65));
}

/** The server name as displayed, led by its country flag when one is known. */
export function serverLabel(server: { name: string; countryCode?: string | null }): string {
  const flag = countryFlag(server.countryCode);
  return flag ? `${flag} ${server.name}` : server.name;
}
