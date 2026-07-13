// cli/args.ts
//
// Pure argument parser for the `patch` CLI. Returns a discriminated
// Command (or an error/help command) with NO side effects, so it is
// unit-testable without touching the network or process streams.
//
// Grammar:
//   patch card [--json]
//   patch chat "<message>" --project <p> [--json]
//   patch task get <id> [--json]
//   patch task cancel <id> [--json]
// Global flags (any command): --url <u>, --token <t>, --help/-h
// Env fallbacks (resolved by main, not here): PATCH_URL, PATCH_TOKEN.

export interface CommonOpts {
  json: boolean;
  url?: string;
  token?: string;
}

export type Command =
  | ({ kind: 'card' } & CommonOpts)
  | ({ kind: 'chat'; message: string; project: string } & CommonOpts)
  | ({ kind: 'task-get'; id: string } & CommonOpts)
  | ({ kind: 'task-cancel'; id: string } & CommonOpts)
  | { kind: 'help' }
  | { kind: 'error'; message: string };

interface Flags {
  json: boolean;
  url?: string;
  token?: string;
  project?: string;
  help: boolean;
  positionals: string[];
}

export function parseArgs(argv: string[]): Command {
  const flags = extractFlags(argv);
  if ('error' in flags) return { kind: 'error', message: flags.error };
  if (flags.help || flags.positionals.length === 0) return { kind: 'help' };

  const common: CommonOpts = { json: flags.json, url: flags.url, token: flags.token };
  const [command, ...rest] = flags.positionals;

  switch (command) {
    case 'card':
      return { kind: 'card', ...common };

    case 'chat': {
      const message = rest[0];
      if (!message) return { kind: 'error', message: 'chat: missing message argument' };
      if (!flags.project) return { kind: 'error', message: 'chat: --project <name> is required' };
      return { kind: 'chat', message, project: flags.project, ...common };
    }

    case 'task': {
      const sub = rest[0];
      const id = rest[1];
      if (sub !== 'get' && sub !== 'cancel') {
        return { kind: 'error', message: `task: expected "get" or "cancel", got "${sub ?? ''}"` };
      }
      if (!id) return { kind: 'error', message: `task ${sub}: missing <id> argument` };
      return { kind: sub === 'get' ? 'task-get' : 'task-cancel', id, ...common };
    }

    default:
      return { kind: 'error', message: `unknown command: "${command}"` };
  }
}

/** Split argv into flags + positionals. `--flag value` and `--flag=value` both work. */
function extractFlags(argv: string[]): Flags | { error: string } {
  const flags: Flags = { json: false, help: false, positionals: [] };

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (arg === '--json') {
      flags.json = true;
    } else if (arg === '--help' || arg === '-h') {
      flags.help = true;
    } else if (arg === '--url' || arg.startsWith('--url=')) {
      const value = valueFor(arg, argv, i);
      if (value.consumedNext) i++;
      if (value.value === undefined) return { error: '--url requires a value' };
      flags.url = value.value;
    } else if (arg === '--token' || arg.startsWith('--token=')) {
      const value = valueFor(arg, argv, i);
      if (value.consumedNext) i++;
      if (value.value === undefined) return { error: '--token requires a value' };
      flags.token = value.value;
    } else if (arg === '--project' || arg.startsWith('--project=')) {
      const value = valueFor(arg, argv, i);
      if (value.consumedNext) i++;
      if (value.value === undefined) return { error: '--project requires a value' };
      flags.project = value.value;
    } else if (arg.startsWith('--')) {
      return { error: `unknown flag: ${arg}` };
    } else {
      flags.positionals.push(arg);
    }
  }
  return flags;
}

function valueFor(
  arg: string,
  argv: string[],
  index: number
): { value: string | undefined; consumedNext: boolean } {
  const eq = arg.indexOf('=');
  if (eq !== -1) return { value: arg.slice(eq + 1), consumedNext: false };
  const next = argv[index + 1];
  if (next === undefined || next.startsWith('--')) return { value: undefined, consumedNext: false };
  return { value: next, consumedNext: true };
}

export const USAGE = `patch — Datum Cloud assistant (A2A) CLI

Usage:
  patch card [--json]
  patch chat "<message>" --project <name> [--json]
  patch task get <id> [--json]
  patch task cancel <id> [--json]

Options:
  --project <name>   Milo project the task runs against (chat)
  --url <url>        Service base URL (overrides PATCH_URL)
  --token <token>    Bearer token (overrides PATCH_TOKEN)
  --json             Emit raw JSON (events for chat, objects otherwise)
  -h, --help         Show this help

Environment:
  PATCH_URL          Service base URL, e.g. http://localhost:7820
  PATCH_TOKEN        Bearer token for the service

Examples:
  PATCH_URL=http://localhost:7820 PATCH_TOKEN=dev-token \\
    patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project
  patch card --url http://localhost:7820
`;
