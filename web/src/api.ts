/**
 * Small, typed browser client for the v1 HTTP contract.
 *
 * The API uses an HttpOnly session cookie, so requests deliberately use
 * credentials: include.  Decimal uint64 values are kept as strings at this
 * boundary; callers must not coerce counters or revisions to Number.
 */

export type ApiErrorBody = {
  code?: string;
  message?: string;
  retryable?: boolean;
  correlation_id?: string;
  fields?: Record<string, string>;
};

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly retryable: boolean;
  readonly correlationId?: string;
  readonly authExpired: boolean;
  readonly retryAfterSeconds?: number;

  constructor(status: number, body: ApiErrorBody | undefined, fallback = 'Request failed', retryAfterSeconds?: number) {
    super(body?.message || fallback);
    this.name = 'ApiError';
    this.status = status;
    this.code = body?.code;
    this.retryable = body?.retryable === true || status >= 500;
    this.correlationId = body?.correlation_id;
    this.authExpired = status === 401;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

export type Server = {
  id: string;
  name: string;
  address?: string;
  role: 'standalone' | 'hub' | 'node' | 'cli-only';
  architecture: string;
  platform: string;
  capabilities: string[];
  version?: string;
  last_heartbeat?: string | null;
  connection_state: 'connected' | 'disconnected' | 'never-connected' | 'revoked';
  freshness_state: 'fresh' | 'stale' | 'unknown';
  freshness_reason?: string;
  configuration_revision: string;
};

export type ServerPage = { items: Server[]; next_cursor?: string };
export type Module = { id: string; name: string; description?: string; latest_version: string; dependencies?: string[]; required_privileges?: string[]; resource_estimate_source: string };
export type ModulePage = { items: Module[]; next_cursor?: string };
export type ModuleInstallation = { server_id: string; module_id: string; version?: string; state: 'unavailable' | 'available' | 'downloading' | 'verifying' | 'installing' | 'installed-disabled' | 'enabled' | 'updating' | 'removing' | 'failed'; revision: string; updated_at: string; error?: ApiErrorBody };
export type ModuleInstallationPage = { items: ModuleInstallation[]; next_cursor?: string };
export type ModuleManifest = Record<string, unknown>;
export type AlertState = { id: string; rule_id: string; server_id?: string; state: 'pending' | 'firing' | 'recovered'; last_observation?: string; last_value?: number; incident_id?: string; precision_warning?: string };
export type AlertStatePage = { items: AlertState[]; next_cursor?: string };

export type MetricSample = {
  server_id: string;
  collector_epoch: string;
  sequence: string;
  observed_at: string;
  received_at: string;
  timestamp_uncertainty?: string;
  values: Record<string, number>;
  counters?: Record<string, string>;
  units?: Record<string, string>;
  validity?: Record<string, string>;
};
export type MetricQuery = {
  samples: MetricSample[];
  rollups: Array<Record<string, unknown>>;
  coverage: Record<string, number>;
  gaps?: Array<Record<string, unknown>>;
  next_cursor?: string;
  gaps_next_cursor?: string;
  truncated?: boolean;
};

export type TrafficPeriod = {
  scope: string;
  interfaces?: string[];
  from: string;
  to: string;
  timezone: string;
  allowance_bytes: string;
  direction: 'inbound' | 'outbound' | 'combined';
  counted_bytes: string;
  continuity: 'complete' | 'gap' | 'uncertain';
};
export type TrafficQuery = { periods: TrafficPeriod[]; next_cursor?: string };

export type LogEntry = {
  source_id: string;
  cursor: string;
  timestamp: string;
  severity?: string;
  text: string;
  truncated?: boolean;
  redacted?: boolean;
};
export type LogQuery = { entries: LogEntry[]; next_cursor?: string; truncated?: boolean };
export type LogSource = { id: string; label: string };
export type LogSourcePage = { items: LogSource[]; next_cursor?: string };

export type QueryOptions = { signal?: AbortSignal };

export type JobState = 'queued' | 'running' | 'cancelling' | 'succeeded' | 'failed' | 'cancelled' | 'recovery-required';
export type Job = {
  id: string;
  kind: string;
  state: JobState;
  revision: string;
  idempotency_key: string;
  target_server_id?: string;
  expires_at: string;
  cancel_requested?: boolean;
  progress: number;
  error?: ApiErrorBody;
};
export type SetupRequest = { setup_secret: string; username: string; password: string };
export type EnrollmentRequest = { token: string; idempotency_key: string };
export type UpdateRequest = { release: string; selected_server_ids: string[]; idempotency_key: string; expires_at?: string };
export type InstallRequest = {
  server_id: string;
  host: string;
  port: number;
  user: string;
  password?: string;
  private_key?: string;
  private_key_passphrase?: string;
  sudo_password?: string;
  expected_host_key_fingerprint?: string;
  role?: 'node' | 'standalone' | 'hub' | 'cli-only';
  listen?: string;
  start?: boolean;
  idempotency_key: string;
  expires_at?: string;
};

/** Run independent reads with a bounded number of active workers. */
export async function mapWithConcurrency<T, R>(
  items: readonly T[],
  concurrency: number,
  signal: AbortSignal,
  mapper: (item: T, signal: AbortSignal) => Promise<R>
): Promise<R[]> {
  if (concurrency < 1 || !Number.isInteger(concurrency)) throw new RangeError('concurrency must be a positive integer');
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async () => {
    while (!signal.aborted) {
      const index = next++;
      if (index >= items.length) return;
      results[index] = await mapper(items[index], signal);
    }
  };
  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, worker));
  if (signal.aborted) throw new DOMException('The request was aborted.', 'AbortError');
  return results;
}

function queryString(params: Record<string, string | number | undefined>): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) if (value !== undefined) query.set(key, String(value));
  const encoded = query.toString();
  return encoded ? `?${encoded}` : '';
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly fetchImpl: typeof fetch;
  private csrfToken = '';

  // Safari requires fetch to be called with its Window/Worker receiver. Keep
  // the injectable implementation for tests, but bind the browser default so
  // a detached method call cannot fail at runtime.
  constructor(baseUrl = import.meta.env.VITE_PAYESH_API_BASE || '/api/v1', fetchImpl: typeof fetch = globalThis.fetch.bind(globalThis)) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.fetchImpl = fetchImpl;
  }

  private async request<T>(path: string, options: RequestInit = {}): Promise<T> {
    let response: Response;
    try {
      response = await this.fetchImpl(`${this.baseUrl}${path}`, {
        ...options,
        credentials: 'include',
        headers: { Accept: 'application/json', ...(options.body ? { 'Content-Type': 'application/json' } : {}), ...options.headers }
      });
    } catch (error) {
      // Abort is intentionally allowed through so stale requests cannot update
      // the view after navigation.
      throw error;
    }
    if (response.ok) {
      const csrf = response.headers.get('X-CSRF-Token');
      if (csrf) this.csrfToken = csrf;
      if (response.status === 204) return undefined as T;
      // Setup returns 201 with an intentionally empty body. Read text first
      // so both empty success responses and JSON responses are supported.
      const payload = await response.text();
      return (payload.trim() ? JSON.parse(payload) : undefined) as T;
    }
    let body: ApiErrorBody | undefined;
    try { body = await response.json() as ApiErrorBody; } catch { /* non-JSON gateway error */ }
    const retryAfter = Number(response.headers.get('Retry-After'));
    throw new ApiError(response.status, body, response.status === 401 ? 'Your session has expired.' : `Request failed (${response.status})`, Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : undefined);
  }

  private mutationHeaders(headers: Record<string, string> = {}): Record<string, string> {
    return { ...(this.csrfToken ? { 'X-CSRF-Token': this.csrfToken } : {}), ...headers };
  }

  completeSetup(body: SetupRequest, options: QueryOptions = {}): Promise<void> {
    return this.request<void>('/setup', { method: 'POST', body: JSON.stringify(body), signal: options.signal });
  }

  login(username: string, password: string, options: QueryOptions = {}): Promise<void> {
    return this.request<void>('/session', { method: 'POST', body: JSON.stringify({ username, password }), signal: options.signal });
  }

  async logout(options: QueryOptions = {}): Promise<void> {
    await this.request<void>('/session', { method: 'DELETE', headers: this.mutationHeaders(), signal: options.signal });
    this.csrfToken = '';
  }

  updateServerLabel(serverId: string, name: string, expectedRevision: string, idempotencyKey: string, options: QueryOptions = {}): Promise<Server> {
    return this.request<Server>(`/servers/${encodeURIComponent(serverId)}`, {
      method: 'PATCH', headers: this.mutationHeaders({ 'Idempotency-Key': idempotencyKey }),
      body: JSON.stringify({ name, expected_revision: expectedRevision }), signal: options.signal
    });
  }

  deleteServer(serverId: string, expectedRevision: string, options: QueryOptions = {}): Promise<void> {
    return this.request<void>(`/servers/${encodeURIComponent(serverId)}`, {
      method: 'DELETE', headers: this.mutationHeaders(),
      body: JSON.stringify({ expected_revision: expectedRevision }), signal: options.signal
    });
  }

  enrollServer(serverId: string, body: EnrollmentRequest, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>(`/servers/${encodeURIComponent(serverId)}/enrollment`, {
      method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify(body), signal: options.signal
    });
  }

  revokeServer(serverId: string, idempotencyKey: string, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>(`/servers/${encodeURIComponent(serverId)}/enrollment`, {
      method: 'DELETE', headers: this.mutationHeaders({ 'Idempotency-Key': idempotencyKey }), signal: options.signal
    });
  }

  getJob(jobId: string, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>(`/jobs/${encodeURIComponent(jobId)}`, { signal: options.signal });
  }

  cancelJob(jobId: string, expectedRevision: string, idempotencyKey: string, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>(`/jobs/${encodeURIComponent(jobId)}`, {
      method: 'POST', headers: this.mutationHeaders(),
      body: JSON.stringify({ idempotency_key: idempotencyKey, expected_revision: expectedRevision }), signal: options.signal
    });
  }

  createUpdate(body: UpdateRequest, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>('/updates', { method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify(body), signal: options.signal });
  }

  enqueueInstall(body: InstallRequest, options: QueryOptions = {}): Promise<Job> {
    return this.request<Job>('/installations', { method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify(body), signal: options.signal });
  }

  listServers(options: QueryOptions = {}): Promise<ServerPage> {
    return this.request<ServerPage>(`/servers${queryString({ limit: 200 })}`, { signal: options.signal });
  }

  createServer(body: { name: string; address: string }, options: QueryOptions = {}): Promise<Server> {
    return this.request<Server>('/servers', { method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify(body), signal: options.signal });
  }

  listModules(options: QueryOptions = {}): Promise<ModulePage> {
    return this.request<ModulePage>(`/modules${queryString({ limit: 200 })}`, { signal: options.signal });
  }

  listServerModules(serverId: string, options: QueryOptions = {}): Promise<ModuleInstallationPage> {
    return this.request<ModuleInstallationPage>(`/servers/${encodeURIComponent(serverId)}/modules`, { signal: options.signal });
  }

  installModule(serverId: string, moduleId: string, body: { manifest: ModuleManifest; manifest_signature_b64: string; archive_base64: string; expected_revision: string; idempotency_key: string }, options: QueryOptions = {}): Promise<ModuleInstallation> {
    return this.request<ModuleInstallation>(`/servers/${encodeURIComponent(serverId)}/modules/${encodeURIComponent(moduleId)}/install`, { method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify(body), signal: options.signal });
  }

  moduleAction(serverId: string, moduleId: string, action: 'enable' | 'disable' | 'remove', expectedRevision: string, idempotencyKey: string, options: QueryOptions = {}): Promise<ModuleInstallation> {
    return this.request<ModuleInstallation>(`/servers/${encodeURIComponent(serverId)}/modules/${encodeURIComponent(moduleId)}/${action}`, { method: 'POST', headers: this.mutationHeaders(), body: JSON.stringify({ expected_revision: expectedRevision, idempotency_key: idempotencyKey }), signal: options.signal });
  }

  listAlerts(options: QueryOptions = {}): Promise<AlertStatePage> {
    return this.request<AlertStatePage>(`/alerts${queryString({ limit: 200 })}`, { signal: options.signal });
  }

  getServer(serverId: string, options: QueryOptions = {}): Promise<Server> {
    return this.request<Server>(`/servers/${encodeURIComponent(serverId)}`, { signal: options.signal });
  }

  queryMetrics(serverId: string, params: { from: string; to: string; resolution?: 'raw' | 'minute' | 'hour'; limit?: number } & QueryOptions): Promise<MetricQuery> {
    return this.request<MetricQuery>(`/servers/${encodeURIComponent(serverId)}/metrics${queryString({ from: params.from, to: params.to, resolution: params.resolution, limit: params.limit ?? 200 })}`, { signal: params.signal });
  }

  queryTraffic(serverId: string, params: { from: string; to: string; scope?: string; limit?: number } & QueryOptions): Promise<TrafficQuery> {
    return this.request<TrafficQuery>(`/servers/${encodeURIComponent(serverId)}/traffic${queryString({ from: params.from, to: params.to, scope: params.scope, limit: params.limit ?? 200 })}`, { signal: params.signal });
  }

  listLogSources(serverId: string, options: QueryOptions = {}): Promise<LogSourcePage> {
    return this.request<LogSourcePage>(`/servers/${encodeURIComponent(serverId)}/logs/sources${queryString({ limit: 200 })}`, { signal: options.signal });
  }

  queryLogs(serverId: string, params: { source: string; from?: string; to?: string; severity?: string; search?: string; limit?: number } & QueryOptions): Promise<LogQuery> {
    return this.request<LogQuery>(`/servers/${encodeURIComponent(serverId)}/logs${queryString({ source: params.source, from: params.from, to: params.to, severity: params.severity, search: params.search, limit: params.limit ?? 200 })}`, { signal: params.signal });
  }
}

export const apiClient = new ApiClient();
