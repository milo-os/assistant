#!/usr/bin/env bun
// cli/main.ts
//
// `patch` — a thin A2A client for the Datum Cloud assistant service,
// proving the "portal is just one client" architecture with a second
// consumer. Built on the shared src/a2a/client.ts (no duplicate protocol
// code). `main` is injectable (argv/env/io/deps) for tests; the bottom of
// the file wires the real process streams.
import { A2AClient, A2AClientError } from '../src/a2a/client';
import { parseArgs, USAGE } from './args';
import { renderCard, renderChat, renderTask, type Io } from './render';

export interface MainDeps {
  /** Injectable fetch (tests point this at an in-process service). */
  fetchImpl?: typeof fetch;
}

export async function main(
  argv: string[],
  env: Record<string, string | undefined>,
  io: Io,
  deps: MainDeps = {}
): Promise<number> {
  const command = parseArgs(argv);

  if (command.kind === 'help') {
    io.out(USAGE);
    return 0;
  }
  if (command.kind === 'error') {
    io.err(`patch: ${command.message}\n\n${USAGE}`);
    return 2;
  }

  const baseUrl = command.url ?? env.PATCH_URL;
  if (!baseUrl) {
    io.err('patch: no service URL — set PATCH_URL or pass --url\n');
    return 2;
  }
  const token = command.token ?? env.PATCH_TOKEN;

  const client = new A2AClient({ baseUrl, token, fetchImpl: deps.fetchImpl });

  try {
    switch (command.kind) {
      case 'card': {
        const card = await client.agentCard();
        renderCard(card, command.json, io);
        return 0;
      }
      case 'chat': {
        const stream = client.messageStream(
          client.buildMessageParams(command.message, command.project)
        );
        return await renderChat(stream, { json: command.json }, io);
      }
      case 'task-get': {
        const task = await client.taskGet(command.id);
        renderTask(task, command.json, io);
        return 0;
      }
      case 'task-cancel': {
        const task = await client.taskCancel(command.id);
        renderTask(task, command.json, io);
        return 0;
      }
    }
  } catch (err) {
    if (err instanceof A2AClientError) {
      const status = err.status ? ` [HTTP ${err.status}]` : err.rpcCode ? ` [rpc ${err.rpcCode}]` : '';
      io.err(`patch: ${err.message}${status}\n`);
      return 1;
    }
    io.err(`patch: ${err instanceof Error ? err.message : String(err)}\n`);
    return 1;
  }
}

if (import.meta.main) {
  const io: Io = {
    out: (text) => process.stdout.write(text),
    err: (text) => process.stderr.write(text),
  };
  main(process.argv.slice(2), process.env, io)
    .then((code) => process.exit(code))
    .catch((err) => {
      process.stderr.write(`patch: fatal: ${err instanceof Error ? err.message : String(err)}\n`);
      process.exit(1);
    });
}
