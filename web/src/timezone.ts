export const DEFAULT_TIMEZONE = 'UTC';

export const COMMON_TIMEZONES = [
  'UTC',
  'Asia/Tehran',
  'Europe/London',
  'Europe/Berlin',
  'Europe/Paris',
  'Europe/Amsterdam',
  'Europe/Moscow',
  'America/New_York',
  'America/Chicago',
  'America/Denver',
  'America/Los_Angeles',
  'America/Toronto',
  'America/Sao_Paulo',
  'Asia/Dubai',
  'Asia/Kolkata',
  'Asia/Singapore',
  'Asia/Tokyo',
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Seoul',
  'Asia/Istanbul',
  'Australia/Sydney',
  'Pacific/Auckland'
];

export function getAvailableTimezones(): string[] {
  try {
    if (typeof Intl !== 'undefined' && 'supportedValuesOf' in Intl) {
      const all = (Intl as unknown as { supportedValuesOf: (key: string) => string[] }).supportedValuesOf('timeZone');
      return Array.from(new Set(['UTC', ...COMMON_TIMEZONES, ...all]));
    }
  } catch {}
  return COMMON_TIMEZONES;
}

export function getSavedTimezone(): string {
  if (typeof window === 'undefined') return DEFAULT_TIMEZONE;
  return window.localStorage.getItem('payesh-timezone') || DEFAULT_TIMEZONE;
}

export function saveTimezone(tz: string): void {
  if (typeof window === 'undefined') return;
  window.localStorage.setItem('payesh-timezone', tz);
}

export function formatTimeInTz(dateOrTimestamp: Date | string | number, tz: string, withSeconds = true): string {
  try {
    const d = typeof dateOrTimestamp === 'number' && dateOrTimestamp < 1e11 ? new Date(dateOrTimestamp * 1000) : new Date(dateOrTimestamp);
    if (isNaN(d.getTime())) return '—';
    return d.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: withSeconds ? '2-digit' : undefined,
      timeZone: tz
    });
  } catch {
    return '—';
  }
}

export function formatDateTimeInTz(dateOrTimestamp: Date | string | number, tz: string): string {
  try {
    const d = typeof dateOrTimestamp === 'number' && dateOrTimestamp < 1e11 ? new Date(dateOrTimestamp * 1000) : new Date(dateOrTimestamp);
    if (isNaN(d.getTime())) return '—';
    return d.toLocaleString([], {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      timeZone: tz
    });
  } catch {
    return '—';
  }
}

export function formatHeartbeatInTz(isoString: string | null | undefined, tz: string): string {
  if (!isoString) return '';
  try {
    const d = new Date(isoString);
    if (isNaN(d.getTime())) return `Heartbeat ${isoString.slice(11, 16)}`;
    const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', timeZone: tz });
    return `Heartbeat ${time}`;
  } catch {
    return `Heartbeat ${isoString.slice(11, 16)}`;
  }
}

export function formatLogTimeInTz(timestamp: string, tz: string): string {
  try {
    const d = new Date(timestamp);
    if (isNaN(d.getTime())) return timestamp.slice(11, 19);
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', timeZone: tz });
  } catch {
    return timestamp.slice(11, 19);
  }
}
