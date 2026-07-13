// src/logger.ts
//
// Minimal structured logger for the standalone service. Writes one JSON
// line per event to stdout/stderr — good enough for a v0 backend and
// trivially parseable by a log collector. The composition module's
// `CompositionLogger` (info/warn) is a structural subset of this, so the
// same instance satisfies both.
export interface Logger {
  debug(message: string, data?: Record<string, unknown>): void;
  info(message: string, data?: Record<string, unknown>): void;
  warn(message: string, data?: Record<string, unknown>): void;
  error(message: string, data?: Record<string, unknown>): void;
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

const LEVEL_ORDER: Record<LogLevel, number> = { debug: 10, info: 20, warn: 30, error: 40 };

export function createLogger(minLevel: LogLevel = 'info'): Logger {
  const threshold = LEVEL_ORDER[minLevel];
  const emit = (level: LogLevel, message: string, data?: Record<string, unknown>): void => {
    if (LEVEL_ORDER[level] < threshold) return;
    const line = JSON.stringify({
      level,
      time: new Date().toISOString(),
      message,
      ...(data ?? {}),
    });
    if (level === 'error' || level === 'warn') {
      // eslint-disable-next-line no-console
      console.error(line);
    } else {
      // eslint-disable-next-line no-console
      console.log(line);
    }
  };
  return {
    debug: (m, d) => emit('debug', m, d),
    info: (m, d) => emit('info', m, d),
    warn: (m, d) => emit('warn', m, d),
    error: (m, d) => emit('error', m, d),
  };
}

/** A logger that discards everything — handy for tests. */
export const silentLogger: Logger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
};
