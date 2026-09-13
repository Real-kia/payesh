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
  return fixtureServers.map((server) => ({ ...server, capabilities: [...server.capabilities] })).sort((a, b) => severity[a.displayState] - severity[b.displayState]);
}
