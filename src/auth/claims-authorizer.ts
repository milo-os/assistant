// src/auth/claims-authorizer.ts
//
// v0 Authorizer: decide from the project grants the Principal already
// carries (dev-token list or OIDC claim). This is the local-slice
// stand-in for production authorization, which will be a
// SubjectAccessReviewAuthorizer issuing a SAR against the Milo control
// plane (resolved by the OpenFGA-backed webhook, with the assistant IAM
// role materialized by catalog IAM fan-out). Both implement the same
// `Authorizer` interface with identical 401/403 semantics, so swapping
// them is a wiring change in auth/index.ts — no call-site churn.
import { AuthError, type Authorizer, type Principal } from './types';

export class ClaimsAuthorizer implements Authorizer {
  async authorizeProject(principal: Principal, projectName: string): Promise<void> {
    const grants = principal.grantedProjects;
    const allowed = grants === '*' || grants.includes(projectName);
    if (!allowed) {
      throw new AuthError(403, `Token does not grant access to project "${projectName}"`);
    }
  }
}
