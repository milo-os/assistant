// src/auth/sar-authorizer.ts
//
// Production authorization seam — NAMED STUB (not wired in v0).
//
// This is where caller authorization moves for production: instead of
// trusting the project grants a credential carries (ClaimsAuthorizer),
// issue a SubjectAccessReview against the Milo control plane, resolved by
// the platform's OpenFGA-backed authorization webhook. The assistant IAM
// role the SAR checks is materialized by the catalog IAM fan-out.
//
// On-behalf-of: downstream calls (the SAR itself, and later the
// control-plane binding source / provider MCP servers) should run under
// the AGENT'S own identity via OAuth 2.0 token exchange (RFC 8693),
// exchanging the caller's token rather than forwarding it raw.
//
// It implements the same `Authorizer` interface as ClaimsAuthorizer with
// identical 401/403 semantics, so it slots into createAuthorizer with no
// change to the A2A protocol layer or any call site.
//
// TODO(agent-framework): implement the SAR call. Sketch:
//   POST {apiUrl}/apis/authorization.k8s.io/v1/subjectaccessreviews
//   { spec: { user: <principal.subject>, resourceAttributes: {
//       group: 'assistant.miloapis.com', resource: 'conversations',
//       verb: 'create', namespace: projectName /* project scope */ } } }
//   using an agent-identity token obtained via RFC 8693 token exchange;
//   allow when status.allowed === true, else throw AuthError(403).
import { AuthError, type Authorizer, type Principal } from './types';

export interface SubjectAccessReviewAuthorizerOptions {
  /** Milo control-plane API base URL the SAR is posted to. */
  apiUrl: string;
  /**
   * Resolves the agent-identity bearer token for the SAR call (RFC 8693
   * token exchange in production). Injected so it stays testable.
   */
  getAgentToken?: (principal: Principal) => Promise<string>;
}

export class SubjectAccessReviewAuthorizer implements Authorizer {
  constructor(private readonly options: SubjectAccessReviewAuthorizerOptions) {}

  async authorizeProject(_principal: Principal, _projectName: string): Promise<void> {
    // Not implemented in v0 — the local slice uses ClaimsAuthorizer.
    // Throwing (rather than silently allowing/denying) makes an
    // accidental production wiring fail loud instead of open.
    throw new AuthError(
      403,
      'SubjectAccessReviewAuthorizer is not implemented yet — use ClaimsAuthorizer for the local slice (see auth/sar-authorizer.ts TODO)'
    );
  }
}
