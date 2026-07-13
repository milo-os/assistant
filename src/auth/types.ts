// src/auth/types.ts
//
// Two SEPARATE seams, deliberately split so production authorization can
// slot in without touching token verification:
//
//   Authenticator — "who are you": a bearer token → a Principal
//     (identity + the raw project grants the credential carries). A bad
//     token throws AuthError(401).
//
//   Authorizer — "may you act on this project": a decision over a
//     Principal + project name, throwing AuthError(403) on deny. It is
//     async so a control-plane call (SubjectAccessReview against Milo,
//     resolved by the platform's OpenFGA-backed webhook) can slot in
//     behind the same interface. The v0 ClaimsAuthorizer decides from
//     the Principal's carried grants; a SubjectAccessReviewAuthorizer
//     would decide from `subject` + a SAR call and ignore those grants.
//
// The Principal holds grants as DATA only — it does not make the
// decision. That is the Authorizer's job.
export interface Principal {
  /** Stable subject identifier (dev: configured subject; oidc: `sub`). */
  subject: string;
  /**
   * Project grants carried by the credential (dev-token list or OIDC
   * claim); '*' means all. Consumed by the v0 ClaimsAuthorizer; opaque
   * to a SAR-based authorizer. NOT itself the authorization decision.
   */
  readonly grantedProjects: string[] | '*';
}

export type AuthStatus = 401 | 403;

export class AuthError extends Error {
  constructor(
    public readonly status: AuthStatus,
    message: string
  ) {
    super(message);
    this.name = 'AuthError';
  }
}

export interface Authenticator {
  /** Resolve a bearer token to a Principal, or throw AuthError(401). */
  authenticate(bearerToken: string): Promise<Principal>;
}

export interface Authorizer {
  /**
   * Decide whether `principal` may act on `projectName`. Resolves on
   * allow; throws AuthError(403) on deny. Async so a control-plane
   * SubjectAccessReview can slot in behind this interface unchanged.
   */
  authorizeProject(principal: Principal, projectName: string): Promise<void>;
}

/** Build a data-only Principal from a fixed project grant list ('*' = all). */
export function principalFromProjects(subject: string, projects: string[]): Principal {
  return { subject, grantedProjects: projects.includes('*') ? '*' : projects };
}
