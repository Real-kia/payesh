import type { PreviewServer } from '../preview/fixtures';

export type LogLevel = 'ERROR' | 'WARN' | 'INFO' | 'DEBUG';

export type FleetLogEntry = {
  id: string;
  serverId: string;
  serverName: string;
  timestamp: string;
  level: LogLevel;
  text: string;
  source: string;
  cursor: string;
};

/**
 * Normalises a stored severity. Entries stored as INFO (or without a level)
 * are re-checked against common error and warning markers in their text, so
 * logs from tools that print everything to one stream still stand out.
 */
export function resolveLogLevel(rawSeverity?: string, text = ''): LogLevel {
  const severity = (rawSeverity || '').trim().toUpperCase();
  if (severity === 'ERROR' || severity === 'ERR' || severity === 'CRITICAL' || severity === 'EMERGENCY' || severity === 'FATAL') return 'ERROR';
  if (severity === 'WARN' || severity === 'WARNING') return 'WARN';
  if (severity === 'DEBUG') return 'DEBUG';

  const lower = text.toLowerCase();
  if (
    lower.includes('sqlite_busy') ||
    lower.includes('database is locked') ||
    lower.includes('status=1/failure') ||
    lower.includes('exit-code') ||
    lower.includes('level=error') ||
    lower.includes('[error]') ||
    lower.includes('error:') ||
    lower.includes('panic:') ||
    lower.includes('fatal:') ||
    lower.includes('critical:') ||
    lower.includes('failed to bind') ||
    lower.includes('packet loss spike') ||
    lower.includes('failed to connect') ||
    lower.includes('connection refused')
  ) return 'ERROR';
  if (
    lower.includes('level=warn') ||
    lower.includes('[warn') ||
    lower.includes('warn:') ||
    lower.includes('warning:') ||
    lower.includes('pressure warning') ||
    lower.includes('exceeded threshold') ||
    lower.includes('high disk i/o wait') ||
    (lower.includes('certificate for') && lower.includes('expires in'))
  ) return 'WARN';
  if (lower.includes('level=debug') || lower.includes('[debug]') || lower.includes('debug:')) return 'DEBUG';
  return 'INFO';
}

const templates: Array<{ offsetSec: number; level: LogLevel; serverIdx: number; text: string; source: string }> = [
  { offsetSec: 15, level: 'INFO', serverIdx: 0, text: 'Telemetry ingest batch processed 48 samples in 1.2ms', source: 'payesh-server' },
  { offsetSec: 42, level: 'INFO', serverIdx: 1, text: 'Agent collected CPU (14.2%), Memory (42.8%), Disk (56.1%)', source: 'payesh-agent' },
  { offsetSec: 95, level: 'WARN', serverIdx: 2, text: 'Memory pressure warning: usage exceeded 85% threshold (86.4%)', source: 'cgroup-monitor' },
  { offsetSec: 140, level: 'INFO', serverIdx: 0, text: 'Health ping received from Worker-Node-Frankfurt (latency: 18ms)', source: 'payesh-server' },
  { offsetSec: 210, level: 'ERROR', serverIdx: 1, text: 'Failed to bind ephemeral socket: address already in use (EADDRINUSE :8081)', source: 'network-monitor' },
  { offsetSec: 320, level: 'WARN', serverIdx: 0, text: 'SSL certificate for payesh.internal expires in 12 days', source: 'webtls' },
  { offsetSec: 450, level: 'INFO', serverIdx: 2, text: 'Log rotation executed: archived /var/log/payesh/agent.err.1', source: 'logrotate' },
  { offsetSec: 600, level: 'INFO', serverIdx: 1, text: 'Transport handshake successful via TLS 1.3 (cipher: TLS_AES_128_GCM_SHA256)', source: 'payesh-agent' },
  { offsetSec: 850, level: 'ERROR', serverIdx: 2, text: 'Upstream gateway 192.168.1.1 packet loss spike: 4.8% packet drop detected', source: 'bandwidth-probe' },
  { offsetSec: 1200, level: 'INFO', serverIdx: 0, text: 'Database hourly compact finished: 0 orphaned rows removed, vacuum complete', source: 'payesh-db' },
  { offsetSec: 1540, level: 'WARN', serverIdx: 1, text: 'High disk I/O wait detected: queue depth 8.2 on /dev/nvme0n1', source: 'disk-stat' },
  { offsetSec: 2100, level: 'INFO', serverIdx: 2, text: 'Systemd service payesh-agent reloaded with PID 18420', source: 'systemd' },
  { offsetSec: 2900, level: 'ERROR', serverIdx: 0, text: 'Database lock contention resolved: SQLite busy handler waited 42ms (SQLITE_BUSY)', source: 'payesh-db' },
  { offsetSec: 3600, level: 'INFO', serverIdx: 1, text: 'Heartbeat cycle healthy: 0 alerts active across 12 targets', source: 'payesh-agent' }
];

/** Deterministic fixture logs for the preview build (enough for several pages). */
export function generatePreviewFleetLogs(servers: Pick<PreviewServer, 'id' | 'name'>[], serverFilter: string): FleetLogEntry[] {
  const now = Date.now();
  const fleet = servers.length > 0 ? servers : [{ id: 'srv-master', name: 'Master-Control-01' }, { id: 'srv-worker-1', name: 'Worker-Node-Frankfurt' }, { id: 'srv-worker-2', name: 'Worker-Node-Helsinki' }];
  const entries: FleetLogEntry[] = [];
  let id = 1;
  for (let cycle = 0; cycle < 12; cycle++) {
    for (const template of templates) {
      const server = fleet[template.serverIdx % fleet.length];
      if (serverFilter !== 'all' && server.id !== serverFilter) continue;
      entries.push({
        id: `preview-log-${id}`,
        serverId: server.id,
        serverName: server.name,
        timestamp: new Date(now - (template.offsetSec + cycle * 3600) * 1000).toISOString(),
        level: resolveLogLevel(template.level, template.text),
        text: cycle > 0 ? `${template.text} (cycle ${cycle + 1})` : template.text,
        source: template.source,
        cursor: String(2000 + id)
      });
      id++;
    }
  }
  return entries;
}
