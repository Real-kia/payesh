import type { PreviewServer } from '../preview/fixtures';

export function canDeleteServer(server: Pick<PreviewServer, 'canDelete' | 'role'>): boolean {
  // Older servers omit this policy; preserve node deletion and protect masters.
  return server.canDelete ?? server.role === 'node';
}
