// src/auth/oidc.ts
//
// AUTH_MODE=oidc: verify a bearer JWT against the issuer's JWKS and check
// the audience. The signing-key resolver (`getKey`) is injected so this
// class is testable with a locally generated key and no live IdP — which
// is exactly how the contract exercises it.
//
// Project authorization: the granted projects are read from a JWT claim
// (default `projects`, configurable via OIDC_PROJECTS_CLAIM). The claim
// may be a string array or a space/comma-delimited string. A token with
// no such claim grants no projects (every project check → 403). This is
// the conservative default; wiring OIDC to a real entitlement source is
// a documented follow-up.
import { AuthError, principalFromProjects } from './types';
import type { Authenticator, Principal } from './types';
import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from 'jose';

export interface OidcAuthenticatorOptions {
  issuer: string;
  audience: string;
  projectsClaim: string;
  /** Resolves the JWT signing key. Prod: a remote JWKS; tests: a local one. */
  getKey: JWTVerifyGetKey;
}

export class OidcAuthenticator implements Authenticator {
  constructor(private readonly opts: OidcAuthenticatorOptions) {}

  async authenticate(bearerToken: string): Promise<Principal> {
    let payload: Record<string, unknown>;
    try {
      const result = await jwtVerify(bearerToken, this.opts.getKey, {
        issuer: this.opts.issuer,
        audience: this.opts.audience,
      });
      payload = result.payload as Record<string, unknown>;
    } catch (err) {
      throw new AuthError(401, `JWT verification failed: ${errorMessage(err)}`);
    }

    const subject = typeof payload.sub === 'string' ? payload.sub : undefined;
    if (!subject) {
      throw new AuthError(401, 'JWT is missing a "sub" claim');
    }

    const projects = extractProjects(payload[this.opts.projectsClaim]);
    return principalFromProjects(subject, projects);
  }
}

/**
 * Build a remote JWKS key resolver. If `jwksUri` is omitted it defaults
 * to `${issuer}/.well-known/jwks.json`; override for IdPs that publish
 * their keys elsewhere (e.g. Keycloak's `/protocol/openid-connect/certs`).
 */
export function remoteJwks(issuer: string, jwksUri?: string): JWTVerifyGetKey {
  const uri = jwksUri ?? `${issuer.replace(/\/$/, '')}/.well-known/jwks.json`;
  return createRemoteJWKSet(new URL(uri));
}

export function extractProjects(claim: unknown): string[] {
  if (Array.isArray(claim)) {
    return claim.filter((v): v is string => typeof v === 'string');
  }
  if (typeof claim === 'string') {
    return claim
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
  }
  return [];
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
