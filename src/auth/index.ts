// src/auth/index.ts
//
// Auth factory: pick the authenticator + authorizer from config, and
// extract bearer tokens from an Authorization header. Authentication
// (identity) and authorization (project decision) are separate seams —
// see auth/types.ts.
import type { Config } from '../config';
import type { Logger } from '../logger';
import { ClaimsAuthorizer } from './claims-authorizer';
import { DevAuthenticator } from './dev';
import { OidcAuthenticator, remoteJwks } from './oidc';
import type { Authenticator, Authorizer } from './types';

export { AuthError } from './types';
export type { Authenticator, Authorizer, Principal } from './types';
export { DevAuthenticator, parseDevTokens } from './dev';
export { OidcAuthenticator, remoteJwks, extractProjects } from './oidc';
export { ClaimsAuthorizer } from './claims-authorizer';
export {
  SubjectAccessReviewAuthorizer,
  type SubjectAccessReviewAuthorizerOptions,
} from './sar-authorizer';

export function createAuthenticator(config: Config, logger?: Logger): Authenticator {
  if (config.auth.mode === 'oidc') {
    // Validated in loadConfig, but narrow for the type checker.
    const issuer = config.auth.oidcIssuer!;
    const audience = config.auth.oidcAudience!;
    logger?.info('auth.mode.oidc', { issuer, audience });
    return new OidcAuthenticator({
      issuer,
      audience,
      projectsClaim: config.auth.oidcProjectsClaim,
      getKey: remoteJwks(issuer),
    });
  }

  const dev = new DevAuthenticator(config.auth.devTokens);
  logger?.info('auth.mode.dev', { tokenCount: dev.size });
  return dev;
}

/**
 * Select the authorizer. v0 always uses the credential-carried grants
 * (ClaimsAuthorizer) for both auth modes. Production would branch here to
 * a SubjectAccessReviewAuthorizer (SAR against Milo) without touching any
 * call site — the `Authorizer` interface is the seam.
 */
export function createAuthorizer(_config: Config, logger?: Logger): Authorizer {
  logger?.info('authz.mode', { type: 'claims' });
  return new ClaimsAuthorizer();
}

/** Extract the token from an `Authorization: Bearer <token>` header. */
export function extractBearerToken(authorization: string | undefined | null): string | undefined {
  if (!authorization) return undefined;
  const match = /^Bearer\s+(.+)$/i.exec(authorization.trim());
  return match?.[1]?.trim() || undefined;
}
