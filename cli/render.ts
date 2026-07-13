// cli/render.ts
//
// Terminal rendering for the CLI. All output goes through an injected
// `Io` (out/err writers) so rendering is unit-testable against recorded
// event streams without touching process.stdout/stderr.
//
// Convention: the assistant's ANSWER text goes to STDOUT; status
// transitions and decoration go to STDERR. So `patch chat … > answer.txt`
// captures just the reply, and pipelines stay clean.
import type { AgentCard } from '../src/a2a/agent-card';
import type { StreamEvent, Task, TaskState, TextPart } from '../src/a2a/types';

export interface Io {
  out(text: string): void;
  err(text: string): void;
}

/** Chat exit codes: 0 completed, 1 failed/canceled/incomplete. */
export async function renderChat(
  events: AsyncIterable<StreamEvent>,
  opts: { json: boolean },
  io: Io
): Promise<number> {
  let finalState: TaskState | undefined;
  let wroteAnswer = false;
  let answerEndsWithNewline = false;

  for await (const event of events) {
    if (opts.json) {
      io.out(`${JSON.stringify(event)}\n`);
    }
    switch (event.kind) {
      case 'task':
        if (!opts.json) io.err(`» task ${event.id} (${event.status.state})\n`);
        break;
      case 'status-update':
        if (!opts.json) io.err(`» ${event.status.state}\n`);
        finalState = event.status.state;
        if (event.status.state === 'failed' && event.status.message) {
          io.err(`  ${textOf(event.status.message.parts)}\n`);
        }
        break;
      case 'artifact-update': {
        const text = textOf(event.artifact.parts);
        if (text && !opts.json) {
          io.out(text);
          wroteAnswer = true;
          answerEndsWithNewline = text.endsWith('\n');
        }
        break;
      }
      case 'message':
        // A bare agent message (non-task path); render its text.
        if (!opts.json) {
          const text = textOf(event.parts);
          if (text) {
            io.out(text);
            wroteAnswer = true;
            answerEndsWithNewline = text.endsWith('\n');
          }
        }
        break;
    }
  }

  // Tidy trailing newline so the shell prompt starts on its own line.
  if (!opts.json && wroteAnswer && !answerEndsWithNewline) io.out('\n');

  return finalState === 'completed' ? 0 : 1;
}

export function renderCard(card: AgentCard, json: boolean, io: Io): void {
  if (json) {
    io.out(`${JSON.stringify(card, null, 2)}\n`);
    return;
  }
  const lines: string[] = [
    `${card.name}  (A2A protocol ${card.protocolVersion}, v${card.version})`,
    card.description,
    '',
    `Endpoint:   ${card.url}  [${card.preferredTransport}]`,
    `Provider:   ${card.provider.organization}  ${card.provider.url}`,
    `Streaming:  ${card.capabilities.streaming ? 'yes' : 'no'}`,
    `Auth:       ${describeSecurity(card)}`,
    `Skills:     ${card.skills.map((s) => s.id).join(', ') || '(none)'}`,
  ];
  io.out(`${lines.join('\n')}\n`);
}

export function renderTask(task: Task, json: boolean, io: Io): void {
  if (json) {
    io.out(`${JSON.stringify(task, null, 2)}\n`);
    return;
  }
  const lines: string[] = [
    `Task ${task.id}`,
    `  context: ${task.contextId}`,
    `  state:   ${task.status.state}`,
  ];
  if (task.status.message) lines.push(`  message: ${textOf(task.status.message.parts)}`);
  const answer = task.artifacts?.flatMap((a) => a.parts) ?? [];
  const answerText = textOf(answer);
  if (answerText) lines.push('', answerText);
  io.out(`${lines.join('\n')}\n`);
}

function textOf(parts: Array<TextPart | { kind: string }>): string {
  return parts
    .filter((p): p is TextPart => p.kind === 'text')
    .map((p) => p.text)
    .join('');
}

function describeSecurity(card: AgentCard): string {
  const scheme = card.securitySchemes?.bearer as { scheme?: string; type?: string } | undefined;
  if (scheme?.type === 'http' && scheme.scheme) return `${scheme.type} ${scheme.scheme}`;
  return Object.keys(card.securitySchemes ?? {}).join(', ') || '(none)';
}
