/** A unique idempotency key for a mutating request, e.g. `install-<uuid>`. */
export function operationKey(prefix: string): string {
  if (typeof crypto === 'undefined') throw new Error('Secure operation identity is unavailable in this browser context.');
  if (typeof crypto.randomUUID === 'function') return `${prefix}-${crypto.randomUUID()}`;
  if (typeof crypto.getRandomValues !== 'function') throw new Error('Secure operation identity is unavailable in this browser context.');
  const random = crypto.getRandomValues(new Uint8Array(16));
  return `${prefix}-${Array.from(random, (value) => value.toString(16).padStart(2, '0')).join('')}`;
}
