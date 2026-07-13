import { parseArgs } from './args';
import { describe, expect, it } from 'bun:test';

describe('parseArgs', () => {
  it('parses `card` with defaults', () => {
    expect(parseArgs(['card'])).toEqual({ kind: 'card', json: false, url: undefined, token: undefined });
  });

  it('parses `card --json` and global flags', () => {
    expect(parseArgs(['card', '--json', '--url', 'http://x', '--token', 't'])).toEqual({
      kind: 'card',
      json: true,
      url: 'http://x',
      token: 't',
    });
  });

  it('supports --flag=value form', () => {
    const cmd = parseArgs(['card', '--url=http://y', '--token=zzz']);
    expect(cmd).toMatchObject({ kind: 'card', url: 'http://y', token: 'zzz' });
  });

  it('parses `chat` with a message and --project', () => {
    expect(parseArgs(['chat', 'hello world', '--project', 'demo'])).toEqual({
      kind: 'chat',
      message: 'hello world',
      project: 'demo',
      json: false,
      url: undefined,
      token: undefined,
    });
  });

  it('errors when chat is missing a message', () => {
    expect(parseArgs(['chat', '--project', 'demo'])).toEqual({
      kind: 'error',
      message: 'chat: missing message argument',
    });
  });

  it('errors when chat is missing --project', () => {
    expect(parseArgs(['chat', 'hi'])).toEqual({
      kind: 'error',
      message: 'chat: --project <name> is required',
    });
  });

  it('parses `task get <id>` and `task cancel <id>`', () => {
    expect(parseArgs(['task', 'get', 't-1'])).toMatchObject({ kind: 'task-get', id: 't-1' });
    expect(parseArgs(['task', 'cancel', 't-2', '--json'])).toMatchObject({
      kind: 'task-cancel',
      id: 't-2',
      json: true,
    });
  });

  it('errors on a bad task subcommand or missing id', () => {
    expect(parseArgs(['task', 'delete', 't'])).toMatchObject({ kind: 'error' });
    expect(parseArgs(['task', 'get'])).toEqual({
      kind: 'error',
      message: 'task get: missing <id> argument',
    });
  });

  it('returns help for no args or --help', () => {
    expect(parseArgs([])).toEqual({ kind: 'help' });
    expect(parseArgs(['--help'])).toEqual({ kind: 'help' });
    expect(parseArgs(['-h'])).toEqual({ kind: 'help' });
  });

  it('errors on an unknown command or flag', () => {
    expect(parseArgs(['frobnicate'])).toEqual({ kind: 'error', message: 'unknown command: "frobnicate"' });
    expect(parseArgs(['card', '--nope'])).toEqual({ kind: 'error', message: 'unknown flag: --nope' });
  });

  it('errors when a value flag is missing its value', () => {
    expect(parseArgs(['card', '--url'])).toEqual({ kind: 'error', message: '--url requires a value' });
  });
});
