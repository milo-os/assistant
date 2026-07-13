import { ConfigError, loadConfig, DEFAULT_PORT } from './config';
import { describe, expect, it } from 'bun:test';

const base = { AUTH_MODE: 'dev', AUTH_DEV_TOKENS: 'tok:sub:projA' };

describe('loadConfig', () => {
  it('applies defaults (port, model mode → mock without a key)', () => {
    const config = loadConfig(base);
    expect(config.port).toBe(DEFAULT_PORT);
    expect(config.model.mode).toBe('mock');
    expect(config.auth.mode).toBe('dev');
    expect(config.publicBaseUrl).toBe(`http://localhost:${DEFAULT_PORT}`);
  });

  it('defaults model mode to anthropic when ANTHROPIC_API_KEY is present', () => {
    const config = loadConfig({ ...base, ANTHROPIC_API_KEY: 'sk-test' });
    expect(config.model.mode).toBe('anthropic');
  });

  it('lets MODEL_MODE=mock win even when a key is present', () => {
    const config = loadConfig({ ...base, ANTHROPIC_API_KEY: 'sk-test', MODEL_MODE: 'mock' });
    expect(config.model.mode).toBe('mock');
  });

  it('rejects MODEL_MODE=anthropic without a key', () => {
    expect(() => loadConfig({ ...base, MODEL_MODE: 'anthropic' })).toThrow(ConfigError);
  });

  it('MODEL_MODE=gateway requires GATEWAY_URL and needs NO model API key', () => {
    expect(() => loadConfig({ ...base, MODEL_MODE: 'gateway' })).toThrow(/GATEWAY_URL/);

    const config = loadConfig({ ...base, MODEL_MODE: 'gateway', GATEWAY_URL: 'http://gw:1975' });
    expect(config.model.mode).toBe('gateway');
    expect(config.model.gatewayUrl).toBe('http://gw:1975');
    expect(config.model.gatewayModel).toBe('patch-stub-v1'); // default
    expect(config.model.anthropicApiKey).toBeUndefined();
  });

  it('parses gateway model + TLS options', () => {
    const config = loadConfig({
      ...base,
      MODEL_MODE: 'gateway',
      GATEWAY_URL: 'https://gw:8443',
      GATEWAY_MODEL: 'patch-stub-v2',
      GATEWAY_CA_CERT: '/etc/certs/gw-ca.pem',
      GATEWAY_TLS_INSECURE: 'true',
    });
    expect(config.model.gatewayModel).toBe('patch-stub-v2');
    expect(config.model.gatewayCaCert).toBe('/etc/certs/gw-ca.pem');
    expect(config.model.gatewayTlsInsecure).toBe(true);
  });

  it('rejects an unknown MODEL_MODE and names all three modes', () => {
    expect(() => loadConfig({ ...base, MODEL_MODE: 'bogus' })).toThrow(/gateway/);
  });

  it('requires AUTH_DEV_TOKENS in dev mode', () => {
    expect(() => loadConfig({ AUTH_MODE: 'dev' })).toThrow(/AUTH_DEV_TOKENS/);
  });

  it('requires issuer+audience in oidc mode', () => {
    expect(() => loadConfig({ AUTH_MODE: 'oidc' })).toThrow(/OIDC_ISSUER/);
    expect(() => loadConfig({ AUTH_MODE: 'oidc', OIDC_ISSUER: 'https://idp' })).toThrow(
      /OIDC_AUDIENCE/
    );
  });

  it('parses a full config', () => {
    const config = loadConfig({
      PORT: '9000',
      PUBLIC_BASE_URL: 'https://assistant.example.com/',
      AUTH_MODE: 'dev',
      AUTH_DEV_TOKENS: 'tok:sub:projA,projB',
      AGENT_BINDINGS_FIXTURE: '/tmp/bindings.json',
      MODEL_MODE: 'mock',
      ANTHROPIC_MODEL: 'claude-test',
      USAGE_GATEWAY_URL: 'http://collector:8080',
      USAGE_GATEWAY_API_KEY: 'k',
    });
    expect(config.port).toBe(9000);
    expect(config.publicBaseUrl).toBe('https://assistant.example.com');
    expect(config.agentBindingsFixture).toBe('/tmp/bindings.json');
    expect(config.model.anthropicModel).toBe('claude-test');
    expect(config.usage.gatewayUrl).toBe('http://collector:8080');
    expect(config.usage.gatewayApiKey).toBe('k');
  });
});
