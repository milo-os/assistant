// src/a2a/types.ts
//
// A2A (Agent2Agent) v1.0 protocol data types — the subset this service
// implements. Field names follow the A2A specification so a conformant
// A2A client can talk to Patch unchanged. Deviations from the spec are
// documented in README.md ("A2A conformance & deviations").
//
// Terminology map for this service:
//   - contextId  = conversation id (also the Conversation metering
//                  resource name)
//   - message.metadata.projectName = Milo project the task runs against
//     (an extension field; documented in the README).

/** A2A message/artifact content parts (text only for v0). */
export interface TextPart {
  kind: 'text';
  text: string;
  metadata?: Record<string, unknown>;
}

export interface DataPart {
  kind: 'data';
  data: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export type Part = TextPart | DataPart;

export type Role = 'user' | 'agent';

/** A2A Message object. */
export interface Message {
  kind: 'message';
  role: Role;
  parts: Part[];
  messageId: string;
  taskId?: string;
  contextId?: string;
  metadata?: Record<string, unknown>;
}

/** A2A task lifecycle states. v0 uses the terminal + working subset. */
export type TaskState =
  | 'submitted'
  | 'working'
  | 'input-required'
  | 'completed'
  | 'canceled'
  | 'failed'
  | 'rejected'
  | 'unknown';

export const TERMINAL_STATES: ReadonlySet<TaskState> = new Set<TaskState>([
  'completed',
  'canceled',
  'failed',
  'rejected',
]);

export function isTerminal(state: TaskState): boolean {
  return TERMINAL_STATES.has(state);
}

export interface TaskStatus {
  state: TaskState;
  /** Optional agent message associated with the status (e.g. final text). */
  message?: Message;
  /** ISO-8601 timestamp of the transition. */
  timestamp: string;
}

export interface Artifact {
  artifactId: string;
  name?: string;
  description?: string;
  parts: Part[];
  metadata?: Record<string, unknown>;
}

/** A2A Task object. */
export interface Task {
  kind: 'task';
  id: string;
  contextId: string;
  status: TaskStatus;
  artifacts?: Artifact[];
  history?: Message[];
  metadata?: Record<string, unknown>;
}

// ── Streaming events (message/stream SSE payloads) ────────────

export interface TaskStatusUpdateEvent {
  kind: 'status-update';
  taskId: string;
  contextId: string;
  status: TaskStatus;
  /** True on the terminal event; the server closes the stream after it. */
  final: boolean;
  metadata?: Record<string, unknown>;
}

export interface TaskArtifactUpdateEvent {
  kind: 'artifact-update';
  taskId: string;
  contextId: string;
  artifact: Artifact;
  /** Append to the named artifact rather than replace it. */
  append?: boolean;
  /** Last chunk for this artifact. */
  lastChunk?: boolean;
  metadata?: Record<string, unknown>;
}

/** Any object a message/stream SSE frame may carry as its JSON-RPC result. */
export type StreamEvent = Task | Message | TaskStatusUpdateEvent | TaskArtifactUpdateEvent;

// ── Method params ─────────────────────────────────────────────

export interface MessageSendConfiguration {
  blocking?: boolean;
  historyLength?: number;
  acceptedOutputModes?: string[];
}

export interface MessageSendParams {
  message: Message;
  configuration?: MessageSendConfiguration;
  metadata?: Record<string, unknown>;
}

export interface TaskQueryParams {
  id: string;
  historyLength?: number;
  metadata?: Record<string, unknown>;
}

export interface TaskIdParams {
  id: string;
  metadata?: Record<string, unknown>;
}
