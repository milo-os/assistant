// src/auth/dev.ts
//
// AUTH_MODE=dev static bearer tokens. Format of AUTH_DEV_TOKENS:
//
//   token:subject:projA,projB;token2:subject2:projC;token3:sub3:*
//
//   - entries separated by ';'
//   - three fields per entry separated by ':' — token, subject, and a
//     comma-separated project grant list
//   - a project grant of '*' grants every project
//
// Whitespace around entries/fields is trimmed. Malformed entries are
// skipped (logged by the caller if it passes a logger). An unknown token
// → AuthError(401); a known token that does not grant the requested
// project → AuthError(403) at the call site.
import { AuthError, principalFromProjects } from './types';
import type { Authenticator, Principal } from './types';

interface DevTokenGrant {
  subject: string;
  projects: string[];
}

export function parseDevTokens(raw: string): Map<string, DevTokenGrant> {
  const map = new Map<string, DevTokenGrant>();
  for (const entry of raw.split(';')) {
    const trimmed = entry.trim();
    if (!trimmed) continue;
    // Split into at most 3 fields; a project list never contains ':'.
    const firstColon = trimmed.indexOf(':');
    const secondColon = trimmed.indexOf(':', firstColon + 1);
    if (firstColon === -1 || secondColon === -1) continue;
    const token = trimmed.slice(0, firstColon).trim();
    const subject = trimmed.slice(firstColon + 1, secondColon).trim();
    const projectsRaw = trimmed.slice(secondColon + 1).trim();
    if (!token || !subject) continue;
    const projects = projectsRaw
      .split(',')
      .map((p) => p.trim())
      .filter((p) => p.length > 0);
    map.set(token, { subject, projects });
  }
  return map;
}

export class DevAuthenticator implements Authenticator {
  private readonly tokens: Map<string, DevTokenGrant>;

  constructor(rawTokens: string) {
    this.tokens = parseDevTokens(rawTokens);
  }

  /** Number of configured tokens (for boot-time logging). */
  get size(): number {
    return this.tokens.size;
  }

  async authenticate(bearerToken: string): Promise<Principal> {
    const grant = this.tokens.get(bearerToken);
    if (!grant) {
      throw new AuthError(401, 'Unknown or invalid bearer token');
    }
    return principalFromProjects(grant.subject, grant.projects);
  }
}
