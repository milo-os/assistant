import { AuthError } from './types';
import { ClaimsAuthorizer } from './claims-authorizer';
import { SubjectAccessReviewAuthorizer } from './sar-authorizer';
import { DevAuthenticator, parseDevTokens } from './dev';
import { OidcAuthenticator, extractProjects } from './oidc';
import { extractBearerToken } from './index';
import { describe, expect, it, beforeAll } from 'bun:test';
import {
  SignJWT,
  createLocalJWKSet,
  exportJWK,
  generateKeyPair,
  type JWK,
  type JWTVerifyGetKey,
} from 'jose';

// ── extractBearerToken ────────────────────────────────────────

describe('extractBearerToken', () => {
  it('pulls the token from an Authorization header', () => {
    expect(extractBearerToken('Bearer abc123')).toBe('abc123');
    expect(extractBearerToken('bearer abc123')).toBe('abc123');
  });
  it('returns undefined for missing/malformed headers', () => {
    expect(extractBearerToken(undefined)).toBeUndefined();
    expect(extractBearerToken('')).toBeUndefined();
    expect(extractBearerToken('Basic abc')).toBeUndefined();
    expect(extractBearerToken('Bearer ')).toBeUndefined();
  });
});

// ── Dev authenticator ─────────────────────────────────────────

describe('parseDevTokens', () => {
  it('parses token:subject:projects entries', () => {
    const map = parseDevTokens('t1:alice:projA,projB;t2:bob:projC');
    expect(map.get('t1')).toEqual({ subject: 'alice', projects: ['projA', 'projB'] });
    expect(map.get('t2')).toEqual({ subject: 'bob', projects: ['projC'] });
  });
  it('supports a wildcard project grant and skips malformed entries', () => {
    const map = parseDevTokens('admin:root:*; garbage ; :nosub:proj');
    expect(map.get('admin')).toEqual({ subject: 'root', projects: ['*'] });
    expect(map.size).toBe(1);
  });
});

describe('DevAuthenticator (identity)', () => {
  const auth = new DevAuthenticator('good:alice:demo-project,other-project;admin:root:*');

  it('resolves a known token to a principal carrying its subject + grants', async () => {
    const principal = await auth.authenticate('good');
    expect(principal.subject).toBe('alice');
    expect(principal.grantedProjects).toEqual(['demo-project', 'other-project']);
  });

  it('carries a wildcard grant as "*"', async () => {
    const principal = await auth.authenticate('admin');
    expect(principal.grantedProjects).toBe('*');
  });

  it('throws AuthError(401) for an unknown token', async () => {
    await expect(auth.authenticate('unknown')).rejects.toBeInstanceOf(AuthError);
    await expect(auth.authenticate('unknown')).rejects.toMatchObject({ status: 401 });
  });
});

// ── ClaimsAuthorizer (the v0 authorization decision) ──────────

describe('ClaimsAuthorizer', () => {
  const auth = new DevAuthenticator('good:alice:demo-project,other-project;admin:root:*');
  const authz = new ClaimsAuthorizer();

  it('allows a granted project (resolves)', async () => {
    const principal = await auth.authenticate('good');
    await expect(authz.authorizeProject(principal, 'demo-project')).resolves.toBeUndefined();
    await expect(authz.authorizeProject(principal, 'other-project')).resolves.toBeUndefined();
  });

  it('denies an ungranted project with AuthError(403)', async () => {
    const principal = await auth.authenticate('good');
    await expect(authz.authorizeProject(principal, 'nope')).rejects.toMatchObject({ status: 403 });
  });

  it('allows any project for a wildcard principal', async () => {
    const principal = await auth.authenticate('admin');
    await expect(authz.authorizeProject(principal, 'anything')).resolves.toBeUndefined();
  });
});

describe('SubjectAccessReviewAuthorizer (named production stub)', () => {
  it('is an Authorizer that fails loud (403) until implemented', async () => {
    const authz = new SubjectAccessReviewAuthorizer({ apiUrl: 'https://milo.example' });
    const principal = { subject: 'user-1', grantedProjects: '*' as const };
    await expect(authz.authorizeProject(principal, 'demo-project')).rejects.toMatchObject({
      status: 403,
    });
  });
});

// ── OIDC authenticator (locally generated key, no live IdP) ────

describe('extractProjects', () => {
  it('reads an array claim', () => {
    expect(extractProjects(['a', 'b', 3])).toEqual(['a', 'b']);
  });
  it('reads a space/comma-delimited string claim', () => {
    expect(extractProjects('a b,c')).toEqual(['a', 'b', 'c']);
  });
  it('returns [] for absent/other claim shapes', () => {
    expect(extractProjects(undefined)).toEqual([]);
    expect(extractProjects(42)).toEqual([]);
  });
});

describe('OidcAuthenticator', () => {
  const ISSUER = 'https://idp.example.com';
  const AUDIENCE = 'assistant.miloapis.com';
  const KID = 'test-key-1';

  let privateKey: CryptoKey;
  let getKey: JWTVerifyGetKey;
  let otherPrivateKey: CryptoKey;

  beforeAll(async () => {
    const pair = await generateKeyPair('RS256', { extractable: true });
    privateKey = pair.privateKey;
    const publicJwk: JWK = { ...(await exportJWK(pair.publicKey)), kid: KID, alg: 'RS256' };
    getKey = createLocalJWKSet({ keys: [publicJwk] });

    const otherPair = await generateKeyPair('RS256', { extractable: true });
    otherPrivateKey = otherPair.privateKey;
  });

  function makeAuth(): OidcAuthenticator {
    return new OidcAuthenticator({
      issuer: ISSUER,
      audience: AUDIENCE,
      projectsClaim: 'projects',
      getKey,
    });
  }

  async function sign(
    payload: Record<string, unknown>,
    opts: { issuer?: string; audience?: string; expSeconds?: number; key?: CryptoKey } = {}
  ): Promise<string> {
    const now = Math.floor(Date.now() / 1000);
    return new SignJWT(payload)
      .setProtectedHeader({ alg: 'RS256', kid: KID })
      .setSubject('user-42')
      .setIssuer(opts.issuer ?? ISSUER)
      .setAudience(opts.audience ?? AUDIENCE)
      .setIssuedAt(now)
      .setExpirationTime(now + (opts.expSeconds ?? 3600))
      .sign(opts.key ?? privateKey);
  }

  it('verifies a valid token and maps the projects claim', async () => {
    const token = await sign({ projects: ['demo-project', 'p2'] });
    const principal = await makeAuth().authenticate(token);
    expect(principal.subject).toBe('user-42');
    expect(principal.grantedProjects).toEqual(['demo-project', 'p2']);
  });

  it('carries no project grants when the claim is absent', async () => {
    const token = await sign({});
    const principal = await makeAuth().authenticate(token);
    expect(principal.grantedProjects).toEqual([]);
  });

  it('rejects a wrong audience (401)', async () => {
    const token = await sign({ projects: ['p'] }, { audience: 'someone-else' });
    await expect(makeAuth().authenticate(token)).rejects.toMatchObject({ status: 401 });
  });

  it('rejects a wrong issuer (401)', async () => {
    const token = await sign({ projects: ['p'] }, { issuer: 'https://evil.example.com' });
    await expect(makeAuth().authenticate(token)).rejects.toMatchObject({ status: 401 });
  });

  it('rejects an expired token (401)', async () => {
    const token = await sign({ projects: ['p'] }, { expSeconds: -10 });
    await expect(makeAuth().authenticate(token)).rejects.toMatchObject({ status: 401 });
  });

  it('rejects a token signed by an unknown key (401)', async () => {
    const token = await sign({ projects: ['p'] }, { key: otherPrivateKey });
    await expect(makeAuth().authenticate(token)).rejects.toMatchObject({ status: 401 });
  });

  it('rejects a garbage token (401)', async () => {
    await expect(makeAuth().authenticate('not-a-jwt')).rejects.toMatchObject({ status: 401 });
  });
});
