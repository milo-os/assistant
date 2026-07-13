// src/index.ts
//
// Boot entry point. Loads config from the environment, wires the app,
// and starts the HTTP server via @hono/node-server (works under both
// Node and Bun). Fails fast with a readable message on bad config.
import { serve } from '@hono/node-server';
import { buildApp } from './server';
import { ConfigError, loadConfig } from './config';
import { createLogger } from './logger';

function main(): void {
  let config;
  try {
    config = loadConfig(process.env);
  } catch (err) {
    if (err instanceof ConfigError) {
      // eslint-disable-next-line no-console
      console.error(err.message);
      process.exit(1);
    }
    throw err;
  }

  const logger = createLogger(config.logLevel);
  const { app } = buildApp(config, logger);

  serve({ fetch: app.fetch, port: config.port, hostname: config.host }, (info) => {
    logger.info('server.listening', {
      port: info.port,
      host: config.host,
      publicBaseUrl: config.publicBaseUrl,
      authMode: config.auth.mode,
      modelMode: config.model.mode,
      agentBindingsFixture: config.agentBindingsFixture ?? null,
      usageGateway: config.usage.gatewayUrl ?? null,
    });
  });
}

main();
