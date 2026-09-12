import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(new URL('../web/package.json', import.meta.url));
const YAML = require('yaml');

const spec = readFileSync(new URL('../api/openapi.yaml', import.meta.url), 'utf8');
const document = YAML.parse(spec);
if (document.openapi !== '3.0.3' || !document.paths || !document.components) {
  throw new Error('OpenAPI document is missing required top-level sections');
}
const walkRefs = (value, refs = []) => {
  if (Array.isArray(value)) value.forEach((item) => walkRefs(item, refs));
  else if (value && typeof value === 'object') {
    if (typeof value.$ref === 'string') refs.push(value.$ref);
    Object.values(value).forEach((item) => walkRefs(item, refs));
  }
  return refs;
};
for (const ref of walkRefs(document)) {
  if (!ref.startsWith('#/')) continue;
  let value = document;
  for (const part of ref.slice(2).split('/')) value = value?.[part];
  if (value === undefined) throw new Error(`unresolved OpenAPI reference: ${ref}`);
}
for (const [path, item] of Object.entries(document.paths)) {
  for (const [method, operation] of Object.entries(item)) {
    if (!['get', 'post', 'put', 'patch', 'delete'].includes(method)) continue;
    const exempt = (path === '/setup' || path === '/session') && method === 'post';
    if (exempt) continue;
    const mutation = ['post', 'put', 'patch', 'delete'].includes(method);
    if (mutation && !operation.security?.some((scheme) => scheme.SessionCookie && scheme.CSRFToken)) {
      throw new Error(`mutation lacks session+CSRF security: ${method} ${path}`);
    }
    if (!mutation && !operation.security?.some((scheme) => scheme.SessionCookie)) {
      throw new Error(`protected operation lacks session security: ${method} ${path}`);
    }
    if (mutation && !operation.parameters?.some((parameter) => parameter.$ref === '#/components/parameters/CSRFHeader')) {
      throw new Error(`mutation lacks CSRF header requirement: ${method} ${path}`);
    }
  }
}
const requiredPaths = [
  '/setup:', '/session:', '/servers:', '/servers/{serverId}:',
  '/servers/{serverId}/metrics:', '/servers/{serverId}/traffic:',
  '/servers/{serverId}/logs:', '/alerts/rules:', '/alerts:',
  '/alerts/history:', '/maintenance-windows:', '/servers/{serverId}/logs/live:',
  '/incidents/{incidentId}:', '/modules:', '/servers/{serverId}/modules:',
  '/servers/{serverId}/modules/{moduleId}/install:',
  '/servers/{serverId}/modules/{moduleId}/enable:',
  '/servers/{serverId}/modules/{moduleId}/disable:',
  '/servers/{serverId}/modules/{moduleId}/remove:',
  '/servers/{serverId}/cpu-policies:', '/releases:', '/updates:', '/settings:',
  '/servers/{serverId}/cpu-policies/{targetKind}/{targetName}/preview:',
  '/servers/{serverId}/cpu-policies/{targetKind}/{targetName}/apply:',
  '/servers/{serverId}/cpu-policies/{targetKind}/{targetName}/revert:',
  '/updates/preflight:', '/backups:', '/backups/export:', '/backups/import:',
  '/role-transitions:', '/servers/{serverId}/enrollment:', '/servers/{serverId}/enrollment-token:', '/jobs/{jobId}:',
  '/audit-events:'
];
for (const path of requiredPaths) {
  if (!spec.includes(`  ${path}`)) throw new Error(`OpenAPI path missing: ${path}`);
}
for (const schema of ['MetricSample:', 'CoverageGap:', 'Job:', 'APIError:', 'CancelJobRequest:', 'TrafficPeriod:', 'Rollup:', 'LogEntry:', 'LiveLogEvent:', 'Module:', 'Policy:', 'Release:', 'ReleaseArtifact:', 'Backup:', 'MaintenanceWindow:', 'ServerPage:', 'LogSourcePage:', 'AlertRulePage:', 'AlertStatePage:', 'ModulePage:', 'ModuleInstallationPage:', 'PolicyPage:', 'ReleasePage:', 'BackupPage:', 'AuditEvent:', 'AuditEventPage:', 'ServerLabelUpdateRequest:', 'EnrollmentTokenRequest:', 'EnrollmentToken:']) {
  if (!spec.includes(`    ${schema}`)) throw new Error(`OpenAPI schema missing: ${schema}`);
}
if (/sequence:\s*\{ type: integer/.test(spec) || /revision:\s*\{ type: integer/.test(spec)) {
  throw new Error('64-bit sequence/revision fields must be decimal strings');
}
if (!spec.includes("pattern: '^(0|[1-9][0-9]{0,18}") || !spec.includes('1844674407370955161[0-5]')) {
  throw new Error('decimal-string pattern missing from OpenAPI uint64 schema');
}
if (!spec.includes("x-max-value: '18446744073709551615'")) {
  throw new Error('uint64 maximum bound missing from OpenAPI');
}
const uint64Pattern = new RegExp(document.components.schemas.DecimalUint64.pattern);
for (const value of ['0', '9007199254740993', '18446744000000000000', '18446744073699999999', '18446744073709551615']) {
  if (!uint64Pattern.test(value)) throw new Error(`uint64 pattern rejects valid value: ${value}`);
}
for (const value of ['00', '18446744073709551616']) {
  if (uint64Pattern.test(value)) throw new Error(`uint64 pattern accepts invalid value: ${value}`);
}
if (!spec.includes('SessionCookie:') || !spec.includes('security:')) {
  throw new Error('protected API operations must declare session authentication');
}
if (!spec.includes('maximum: 200') || !spec.includes('x-max-duration: P31D')) {
  throw new Error('bounded query pagination/range is missing');
}
if (document['x-max-request-bytes'] !== 1048576 || document['x-max-response-bytes'] !== 1048576) {
  throw new Error('request/response byte bounds missing');
}
const arrays = [];
const collectArrays = (value, path = '') => {
  if (!value || typeof value !== 'object') return;
  if (value.type === 'array') arrays.push([path, value]);
  Object.entries(value).forEach(([key, item]) => collectArrays(item, `${path}/${key}`));
};
collectArrays(document.components.schemas, '#/components/schemas');
for (const [path, schema] of arrays) {
  if (schema.maxItems === undefined) throw new Error(`array is unbounded: ${path}`);
}
if (!spec.includes('required: [idempotency_key, expected_revision]')) {
  throw new Error('job cancellation must be idempotent and revision-checked');
}
const login = document.paths['/session']?.post;
if (!login?.responses?.['429']?.$ref?.endsWith('/TooManyRequests') || document.components.headers?.RetryAfter?.schema?.minimum !== 1) {
  throw new Error('login throttle response or Retry-After contract missing');
}
const labelUpdate = document.paths['/servers/{serverId}']?.patch;
if (!labelUpdate?.parameters?.some((parameter) => parameter.$ref === '#/components/parameters/IdempotencyKey') || labelUpdate.requestBody?.content?.['application/json']?.schema?.$ref !== '#/components/schemas/ServerLabelUpdateRequest') {
  throw new Error('server label update must be idempotent and revision-checked');
}
const server = document.components.schemas.Server;
if (!server.required?.includes('connection_state') || !server.required?.includes('freshness_state')) {
  throw new Error('server connection/freshness contract missing');
}
console.log('OpenAPI foundation contract checks passed');
