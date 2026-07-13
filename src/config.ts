// src/config.ts
//
// Single place that reads the environment. Everything downstream takes
// the parsed `Config` by injection, so no other module touches
// process.env — that keeps the agent loop, auth, and usage emitter
// harness-drivable.
import type { LogLevel } from './logger';

export type AuthMode = 'dev' | 'oidc';
export type ModelMode = 'anthropic' | 'mock' | 'gateway';

export interface Config {
  port: number;
  host: string;
  /** Public base URL used for the agent-card `url` and CloudEvents `source`. */
  publicBaseUrl: string;
  logLevel: LogLevel;

  auth: {
    mode: AuthMode;
    /** Raw AUTH_DEV_TOKENS string (parsed by the dev authenticator). */
    devTokens: string;
    oidcIssuer?: string;
    oidcAudience?: string;
    /** JWT claim carrying the granted project names (oidc mode). */
    oidcProjectsClaim: string;
  };

  agentBindingsFixture?: string;

  model: {
    mode: ModelMode;
    anthropicApiKey?: string;
    anthropicModel: string;
    // AI gateway (MODEL_MODE=gateway). Distinct from `usage.gatewayUrl`
    // (the metering collector). In this mode the service holds NO upstream
    // model credential — the gateway injects it (BackendSecurityPolicy).
    /** Envoy AI Gateway base URL (OpenAI-compatible endpoint). */
    gatewayUrl?: string;
    /** Model name routed by the gateway to the upstream (e.g. patch-stub-v1). */
    gatewayModel: string;
    /** Optional CA PEM path for a self-signed gateway TLS cert (local). */
    gatewayCaCert?: string;
    /** Skip gateway TLS verification (local convenience only). */
    gatewayTlsInsecure: boolean;
  };

  usage: {
    gatewayUrl?: string;
    gatewayApiKey?: string;
  };
}

export const DEFAULT_PORT = 7820;
export const DEFAULT_ANTHROPIC_MODEL = 'claude-sonnet-4-6';
export const DEFAULT_GATEWAY_MODEL = 'patch-stub-v1';

export interface LoadConfigError {
  field: string;
  message: string;
}

export class ConfigError extends Error {
  constructor(public readonly errors: LoadConfigError[]) {
    super(`Invalid configuration:\n${errors.map((e) => `  - ${e.field}: ${e.message}`).join('\n')}`);
    this.name = 'ConfigError';
  }
}

type RawEnv = Record<string, string | undefined>;

export function loadConfig(env: RawEnv = process.env): Config {
  const errors: LoadConfigError[] = [];

  const port = parseIntOr(env.PORT, DEFAULT_PORT);
  if (Number.isNaN(port) || port <= 0 || port > 65535) {
    errors.push({ field: 'PORT', message: `must be a valid TCP port, got "${env.PORT}"` });
  }

  const host = env.HOST?.trim() || '0.0.0.0';
  const publicBaseUrl = (env.PUBLIC_BASE_URL?.trim() || `http://localhost:${port}`).replace(
    /\/$/,
    ''
  );

  const logLevel = oneOf<LogLevel>(env.LOG_LEVEL, ['debug', 'info', 'warn', 'error'], 'info');

  // ── Auth ──────────────────────────────────────────────────
  const authMode = oneOf<AuthMode>(env.AUTH_MODE, ['dev', 'oidc'], 'dev');
  if (authMode === 'dev' && !env.AUTH_DEV_TOKENS?.trim()) {
    errors.push({
      field: 'AUTH_DEV_TOKENS',
      message: 'AUTH_MODE=dev requires at least one token (format "token:subject:projA,projB;...")',
    });
  }
  if (authMode === 'oidc') {
    if (!env.OIDC_ISSUER?.trim()) {
      errors.push({ field: 'OIDC_ISSUER', message: 'AUTH_MODE=oidc requires OIDC_ISSUER' });
    }
    if (!env.OIDC_AUDIENCE?.trim()) {
      errors.push({ field: 'OIDC_AUDIENCE', message: 'AUTH_MODE=oidc requires OIDC_AUDIENCE' });
    }
  }

  // ── Model ─────────────────────────────────────────────────
  const anthropicApiKey = env.ANTHROPIC_API_KEY?.trim() || undefined;
  const gatewayUrl = env.GATEWAY_URL?.trim() || undefined;
  // Default: anthropic when a key is present, else mock. `gateway` is only
  // selected explicitly. An explicit MODEL_MODE always wins.
  const modelModeRaw = env.MODEL_MODE?.trim();
  let modelMode: ModelMode;
  if (modelModeRaw === 'anthropic' || modelModeRaw === 'mock' || modelModeRaw === 'gateway') {
    modelMode = modelModeRaw;
  } else if (modelModeRaw) {
    errors.push({
      field: 'MODEL_MODE',
      message: `must be "anthropic", "mock", or "gateway", got "${modelModeRaw}"`,
    });
    modelMode = 'mock';
  } else {
    modelMode = anthropicApiKey ? 'anthropic' : 'mock';
  }
  if (modelMode === 'anthropic' && !anthropicApiKey) {
    errors.push({
      field: 'ANTHROPIC_API_KEY',
      message: 'MODEL_MODE=anthropic requires ANTHROPIC_API_KEY',
    });
  }
  if (modelMode === 'gateway' && !gatewayUrl) {
    errors.push({
      field: 'GATEWAY_URL',
      message: 'MODEL_MODE=gateway requires GATEWAY_URL (the Envoy AI Gateway endpoint)',
    });
  }

  if (errors.length > 0) throw new ConfigError(errors);

  return {
    port,
    host,
    publicBaseUrl,
    logLevel,
    auth: {
      mode: authMode,
      devTokens: env.AUTH_DEV_TOKENS?.trim() ?? '',
      oidcIssuer: env.OIDC_ISSUER?.trim() || undefined,
      oidcAudience: env.OIDC_AUDIENCE?.trim() || undefined,
      oidcProjectsClaim: env.OIDC_PROJECTS_CLAIM?.trim() || 'projects',
    },
    agentBindingsFixture: env.AGENT_BINDINGS_FIXTURE?.trim() || undefined,
    model: {
      mode: modelMode,
      anthropicApiKey,
      anthropicModel: env.ANTHROPIC_MODEL?.trim() || DEFAULT_ANTHROPIC_MODEL,
      gatewayUrl,
      gatewayModel: env.GATEWAY_MODEL?.trim() || DEFAULT_GATEWAY_MODEL,
      gatewayCaCert: env.GATEWAY_CA_CERT?.trim() || undefined,
      gatewayTlsInsecure: /^(1|true|yes)$/i.test(env.GATEWAY_TLS_INSECURE?.trim() ?? ''),
    },
    usage: {
      gatewayUrl: env.USAGE_GATEWAY_URL?.trim() || undefined,
      gatewayApiKey: env.USAGE_GATEWAY_API_KEY?.trim() || undefined,
    },
  };
}

function parseIntOr(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === '') return fallback;
  return Number.parseInt(value, 10);
}

function oneOf<T extends string>(value: string | undefined, allowed: T[], fallback: T): T {
  const v = value?.trim();
  return allowed.includes(v as T) ? (v as T) : fallback;
}
