import type { ApiToken } from '../api';

export function tokenStatus(token: Pick<ApiToken, 'expires_at'>, now = Date.now()): 'active' | 'expiring' | 'expired' {
  const remaining = new Date(token.expires_at).getTime() - now;
  return remaining <= 0 ? 'expired' : remaining <= 7 * 86400000 ? 'expiring' : 'active';
}

export function filterTokens(tokens: ApiToken[], username: string, search: string): ApiToken[] {
  const query = search.trim().toLowerCase();
  return tokens.filter((token) => (!username || token.username === username) && `${token.name} ${token.username} ${token.hint}`.toLowerCase().includes(query));
}
