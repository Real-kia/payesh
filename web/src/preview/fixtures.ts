export type DisplayState = 'healthy' | 'stale' | 'pending' | 'unreachable' | 'installing' | 'failed' | 'disabled' | 'unsupported';
export type ConnectionState = 'connected' | 'disconnected' | 'never-connected' | 'revoked';
export type FreshnessState = 'fresh' | 'stale' | 'unknown';
export type PreviewChartRange = '15m' | '1h' | '24h';
export type PreviewChartData = {
  timestamps: number[];
  cpu: Array<number | null>;
  memory: Array<number | null>;
  disk: Array<number | null>;
  networkRx?: Array<number | null>;
  networkTx?: Array<number | null>;
  coverage: 'complete' | 'gap' | 'unavailable';
};
export type PreviewMetricHistory = { ranges: Record<PreviewChartRange, PreviewChartData> };

export type PreviewServer = {
  id: string;
  name: string;
  address?: string;
  role: 'standalone' | 'hub' | 'node' | 'cli-only';
  architecture: string;
  platform: string;
  capabilities: string[];
  version: string;
  lastHeartbeat: string | null;
  connectionState: ConnectionState;
  freshnessState: FreshnessState;
  freshnessReason?: string;
  configurationRevision: string;
  displayState: DisplayState;
  metrics: { cpu: number | null; memory: number | null; disk: number | null };
  latestMetricAt?: string;
  metricHistory?: PreviewMetricHistory;
  traffic: { scope: string; from: string; to: string; timezone: string; allowanceBytes: string; direction: 'inbound' | 'outbound' | 'combined'; countedBytes: string; continuity: 'complete' | 'gap' | 'uncertain' };
};

const severity: Record<DisplayState, number> = { stale: 0, unreachable: 1, failed: 2, installing: 3, pending: 4, disabled: 5, unsupported: 6, healthy: 7 };

// This adapter is preview-only. Its values follow the approved API shape,
// including decimal-string IDs/revisions/byte counters, so later API wiring
// replaces one boundary instead of changing the view model.
const fixtureServers: PreviewServer[] = [
  { id: 'server-us-00000002', name: 'Ashburn API', role: 'node', architecture: 'amd64', platform: 'Ubuntu 24.04', capabilities: ['metrics', 'logs'], version: '0.1.0', lastHeartbeat: '2026-09-09T14:34:18Z', connectionState: 'connected', freshnessState: 'stale', freshnessReason: 'heartbeat older than 90 seconds', configurationRevision: '11', displayState: 'stale', metrics: { cpu: 74, memory: 83, disk: 71 }, metricHistory: { ranges: { '15m': { timestamps: [0, 180, 360, 540, 720, 900], cpu: [41, 58, 62, 70, 75, 74], memory: [69, 71, 75, 79, 84, 83], disk: [52, 58, 61, 66, 70, 71], coverage: 'gap' }, '1h': { timestamps: [0, 720, 1440, 2160, 2880, 3600], cpu: [36, 44, 51, 68, 75, 74], memory: [62, 68, 72, 81, 84, 83], disk: [44, 51, 57, 64, 69, 71], coverage: 'gap' }, '24h': { timestamps: [0, 17280, 34560, 51840, 69120, 86400], cpu: [31, 48, 57, 63, 75, 74], memory: [58, 66, 73, 79, 84, 83], disk: [37, 49, 58, 64, 69, 71], coverage: 'gap' } } }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '1000000000000', direction: 'combined', countedBytes: '884000000000', continuity: 'uncertain' } },
  { id: 'server-eu-00000001', name: 'Frankfurt edge', role: 'standalone', architecture: 'amd64', platform: 'Debian 12', capabilities: ['metrics', 'logs', 'traffic'], version: '0.1.0', lastHeartbeat: '2026-09-09T14:42:18Z', connectionState: 'connected', freshnessState: 'fresh', configurationRevision: '12', displayState: 'healthy', metrics: { cpu: 24, memory: 61, disk: 38 }, metricHistory: { ranges: { '15m': { timestamps: [0, 180, 360, 540, 720, 900], cpu: [38, 42, 31, 74, 51, 24], memory: [65, 69, 62, 83, 76, 61], disk: [29, 32, 35, 42, 41, 38], coverage: 'complete' }, '1h': { timestamps: [0, 720, 1440, 2160, 2880, 3600], cpu: [48, 39, 42, 68, 41, 24], memory: [69, 71, 66, 83, 74, 61], disk: [31, 33, 38, 45, 42, 38], coverage: 'complete' }, '24h': { timestamps: [0, 17280, 34560, 51840, 69120, 86400], cpu: [27, 35, 45, 62, 38, 24], memory: [56, 64, 70, 83, 72, 61], disk: [24, 29, 34, 41, 42, 38], coverage: 'complete' } } }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '1000000000000', direction: 'combined', countedBytes: '642000000000', continuity: 'complete' } },
  { id: 'server-sg-00000003', name: 'Singapore worker', role: 'node', architecture: 'arm64', platform: 'Alpine 3.22', capabilities: ['metrics'], version: '0.1.0', lastHeartbeat: null, connectionState: 'never-connected', freshnessState: 'unknown', freshnessReason: 'enrollment is awaiting confirmation', configurationRevision: '0', displayState: 'pending', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'uncertain' } },
  { id: 'server-ap-00000004', name: 'Tokyo backup', role: 'node', architecture: 'arm64', platform: 'Rocky 9', capabilities: ['metrics'], version: '0.1.0', lastHeartbeat: null, connectionState: 'disconnected', freshnessState: 'unknown', freshnessReason: 'transport disconnected', configurationRevision: '4', displayState: 'unreachable', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'gap' } },
  { id: 'server-ca-00000005', name: 'Montreal worker', role: 'node', architecture: 'amd64', platform: 'Fedora 41', capabilities: ['metrics'], version: '0.1.0', lastHeartbeat: null, connectionState: 'connected', freshnessState: 'unknown', freshnessReason: 'health check failed', configurationRevision: '7', displayState: 'failed', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'uncertain' } },
  { id: 'server-uk-00000006', name: 'London staging', role: 'node', architecture: 'amd64', platform: 'Debian 12', capabilities: ['metrics'], version: '0.1.0', lastHeartbeat: null, connectionState: 'connected', freshnessState: 'unknown', configurationRevision: '3', displayState: 'installing', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'uncertain' } },
  { id: 'server-in-00000007', name: 'Mumbai retired', role: 'node', architecture: 'arm64', platform: 'AlmaLinux 9', capabilities: ['metrics'], version: '0.1.0', lastHeartbeat: null, connectionState: 'revoked', freshnessState: 'unknown', freshnessReason: 'disabled by owner', configurationRevision: '9', displayState: 'disabled', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'complete' } },
  { id: 'server-br-00000008', name: 'São Paulo legacy', role: 'node', architecture: 'amd64', platform: 'Unknown Linux', capabilities: [], version: '0.0.0', lastHeartbeat: null, connectionState: 'connected', freshnessState: 'unknown', freshnessReason: 'required capability unavailable', configurationRevision: '2', displayState: 'unsupported', metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z', timezone: 'UTC', allowanceBytes: '500000000000', direction: 'combined', countedBytes: '0', continuity: 'uncertain' } }
];

export type PreviewLogEntry = { time: string; level: 'INFO' | 'WARN' | 'ERROR'; text: string; source: string; cursor: string };

export const previewLogEntries: PreviewLogEntry[] = [
  { time: '14:42:18', level: 'INFO', text: 'collector heartbeat accepted', source: 'payesh-agent', cursor: 'cursor-8842' },
  { time: '14:41:59', level: 'WARN', text: 'source clock is 1.2s ahead of hub; timestamps preserved', source: 'metrics', cursor: 'cursor-8841' },
  { time: '14:40:11', level: 'INFO', text: 'disk usage sample committed (38%)', source: 'metrics', cursor: 'cursor-8837' },
  { time: '14:38:04', level: 'INFO', text: 'configuration revision 12 applied', source: 'payesh-agent', cursor: 'cursor-8830' }
];

export function getPreviewServers(): PreviewServer[] {
  for (const server of fixtureServers) {
    if (!server.metricHistory) continue;
    for (const range of Object.values(server.metricHistory.ranges)) {
      range.networkRx = [800000, 1200000, 900000, 2100000, 1400000, 1250000];
      range.networkTx = [300000, 500000, 450000, 900000, 700000, 625000];
    }
  }
  return fixtureServers.map((server) => ({ ...server, capabilities: [...server.capabilities] })).sort((a, b) => severity[a.displayState] - severity[b.displayState]);
}

// Settings fixtures so every settings section can be reviewed in the preview.
export const previewUpdateStatus = {
  current: '0.3.0',
  latest: '0.3.2',
  update_available: true,
  url: 'https://github.com/Real-kia/payesh/releases/tag/v0.3.2',
  web_update_supported: true,
  web_update: null,
  releases: [
    { version: '0.3.2', url: 'https://github.com/Real-kia/payesh/releases/tag/v0.3.2', published_at: '2026-10-06T10:00:00Z' },
    { version: '0.3.1', url: 'https://github.com/Real-kia/payesh/releases/tag/v0.3.1', published_at: '2026-09-28T10:00:00Z' },
    { version: '0.3.0', url: 'https://github.com/Real-kia/payesh/releases/tag/v0.3.0', published_at: '2026-09-20T10:00:00Z' },
    { version: '0.2.18', url: 'https://github.com/Real-kia/payesh/releases/tag/v0.2.18', published_at: '2026-09-02T10:00:00Z' }
  ]
};

export const previewStorageStatus = {
  settings: { max_database_bytes: 1000000000, sample_seconds: 15, pressure_sample_seconds: 60, adaptive_sampling: true, notifications_enabled: true, revision: '3', pressure_state: 'normal' },
  database_bytes: 412000000,
  recovery_snapshot_bytes: 38000000,
  effective_sample_seconds: 15
};

export const previewApiTokens = [
  { id: 'tok-claude', name: 'Claude Code', username: 'admin', permission: 'read' as const, hint: 'pyt_Q2x9', created_at: '2026-09-30T09:12:00Z', expires_at: '2026-12-29T09:12:00Z', last_used_at: '2026-10-09T17:45:00Z', last_used_ip: '192.0.2.10', status: 'active' as const, server_ids: [], actions: ['monitoring'] },
  { id: 'tok-deploy', name: 'deploy script', username: 'admin', permission: 'edit' as const, hint: 'pyt_7fKd', created_at: '2026-08-14T15:30:00Z', expires_at: '2027-08-14T15:30:00Z', status: 'active' as const, server_ids: [], actions: [] },
  { id: 'tok-ops', name: 'on-call bot', username: 'ops', permission: 'read' as const, hint: 'pyt_m3Rv', created_at: '2026-10-01T08:00:00Z', expires_at: '2026-10-14T08:00:00Z', last_used_at: '2026-10-09T21:02:00Z', last_used_ip: '198.51.100.24', status: 'expiring' as const, server_ids: [], actions: ['monitoring'] }
];

export const previewNotifications = [
  { id: 'n-3', kind: 'storage_warning', message: 'The monitoring database is at 82% of its 1 GB limit. Old history will be compacted soon.', created_at: '2026-09-09T13:10:00Z', read: false },
  { id: 'n-2', kind: 'storage_cleanup', message: 'Removed 3 days of full-resolution samples older than 24 hours; hourly summaries are kept.', created_at: '2026-09-08T03:00:00Z', read: true },
  { id: 'n-1', kind: 'storage_recovered', message: 'Storage pressure cleared. Normal 15-second sampling resumed.', created_at: '2026-09-07T18:40:00Z', read: true }
];

export const previewProcesses = [
  { pid: 1, name: 'systemd', uid: 0, state: 'S', threads: 1, memory_bytes: '13107200', cpu_percent: 0.1, read_bytes_per_second: 0, write_bytes_per_second: 0, connections: 4 },
  { pid: 412, name: 'payesh-server', uid: 998, state: 'S', threads: 14, memory_bytes: '58720256', cpu_percent: 1.8, read_bytes_per_second: 0, write_bytes_per_second: 20480, connections: 12 },
  { pid: 418, name: 'payesh-agent', uid: 998, state: 'S', threads: 9, memory_bytes: '20971520', cpu_percent: 0.6, read_bytes_per_second: 4096, write_bytes_per_second: 0, connections: 1 },
  { pid: 902, name: 'nginx', uid: 33, state: 'S', threads: 4, memory_bytes: '41943040', cpu_percent: 6.4, read_bytes_per_second: 131072, write_bytes_per_second: 8192, connections: 186 },
  { pid: 1210, name: 'postgres', uid: 70, state: 'S', threads: 7, memory_bytes: '402653184', cpu_percent: 12.3, read_bytes_per_second: 2097152, write_bytes_per_second: 1048576, connections: 32 },
  { pid: 1311, name: 'node /srv/api/server.js', uid: 1000, state: 'R', threads: 11, memory_bytes: '268435456', cpu_percent: 27.9, read_bytes_per_second: 65536, write_bytes_per_second: 32768, connections: 74 },
  { pid: 2077, name: 'sshd', uid: 0, state: 'S', threads: 1, memory_bytes: '8388608', cpu_percent: 0, read_bytes_per_second: null, write_bytes_per_second: null, connections: 2 },
  { pid: 3120, name: 'redis-server', uid: 999, state: 'S', threads: 5, memory_bytes: '75497472', cpu_percent: 3.1, read_bytes_per_second: 0, write_bytes_per_second: 4096, connections: 21 }
];
