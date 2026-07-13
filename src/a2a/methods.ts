// src/a2a/methods.ts
//
// A2A method handlers: message/send, message/stream, tasks/get,
// tasks/cancel. Each takes already-parsed params plus the authenticated
// Principal. Project authorization (may this principal act on
// message.metadata.projectName?) is delegated to the injected Authorizer
// and surfaces as AuthError(403); protocol-level problems surface as
// JsonRpcError. Authentication (who) happens upstream in the HTTP layer;
// authorization (may) happens here — the two are separate seams.
import { runAgent, type AgentDeps, type AgentResult } from '../agent';
import { type Authorizer, type Principal } from '../auth';
import type { Logger } from '../logger';
import {
  JsonRpcError,
  A2A_TASK_NOT_FOUND,
  A2A_TASK_NOT_CANCELABLE,
  JSONRPC_INVALID_PARAMS,
} from './jsonrpc';
import { InMemoryTaskStore, isCancelable, type TaskRecord, type TaskStore } from './tasks';
import {
  isTerminal,
  type Artifact,
  type Message,
  type MessageSendParams,
  type StreamEvent,
  type Task,
  type TaskIdParams,
  type TaskQueryParams,
  type TaskState,
  type TaskStatus,
  type TaskStatusUpdateEvent,
} from './types';

export interface A2AServiceDeps {
  agentDeps: AgentDeps;
  authorizer: Authorizer;
  logger: Logger;
  taskStore?: TaskStore;
  /** Injectable id generator (tests). Defaults to crypto.randomUUID. */
  generateId?: () => string;
  /** Injectable clock (tests). Defaults to () => new Date().toISOString(). */
  now?: () => string;
}

const RESPONSE_ARTIFACT_ID = 'response';
const RESPONSE_ARTIFACT_NAME = 'response';

export class A2AService {
  private readonly taskStore: TaskStore;
  private readonly agentDeps: AgentDeps;
  private readonly authorizer: Authorizer;
  private readonly logger: Logger;
  private readonly generateId: () => string;
  private readonly now: () => string;

  constructor(deps: A2AServiceDeps) {
    this.taskStore = deps.taskStore ?? new InMemoryTaskStore();
    this.agentDeps = deps.agentDeps;
    this.authorizer = deps.authorizer;
    this.logger = deps.logger;
    this.generateId = deps.generateId ?? (() => crypto.randomUUID());
    this.now = deps.now ?? (() => new Date().toISOString());
  }

  getTaskStore(): TaskStore {
    return this.taskStore;
  }

  // ── message/send ────────────────────────────────────────────

  async messageSend(params: MessageSendParams, principal: Principal): Promise<Task> {
    const parsed = this.validateMessageParams(params);
    await this.authorizer.authorizeProject(principal, parsed.projectName);
    const record = await this.createTaskRecord(parsed, principal);

    await this.transition(record.task.id, 'working');
    const gen = runAgent(
      {
        userText: parsed.userText,
        projectName: parsed.projectName,
        contextId: record.task.contextId,
        taskId: record.task.id,
        isCanceled: () => this.taskStore.isCancelRequested(record.task.id),
      },
      this.agentDeps
    );

    // Drain the generator; we don't stream in the blocking path.
    let result: AgentResult;
    for (;;) {
      const next = await gen.next();
      if (next.done) {
        result = next.value;
        break;
      }
    }

    return this.finalize(record.task.id, result);
  }

  // ── message/stream ──────────────────────────────────────────

  async *messageStream(
    params: MessageSendParams,
    principal: Principal
  ): AsyncGenerator<StreamEvent> {
    const parsed = this.validateMessageParams(params);
    await this.authorizer.authorizeProject(principal, parsed.projectName);
    const record = await this.createTaskRecord(parsed, principal);
    const { id: taskId, contextId } = record.task;

    // First frame: the initial Task (submitted).
    yield record.task;

    // working
    const working = await this.transition(taskId, 'working');
    yield this.statusEvent(taskId, contextId, working.task.status, false);

    const gen = runAgent(
      {
        userText: parsed.userText,
        projectName: parsed.projectName,
        contextId,
        taskId,
        isCanceled: () => this.taskStore.isCancelRequested(taskId),
      },
      this.agentDeps
    );

    let emittedText = false;
    let result: AgentResult;
    for (;;) {
      const next = await gen.next();
      if (next.done) {
        result = next.value;
        break;
      }
      const event = next.value;
      if (event.type === 'text-delta' && event.text) {
        yield this.artifactEvent(taskId, contextId, event.text, emittedText, false);
        emittedText = true;
      }
      // tool-call events are intentionally not surfaced as SSE frames in
      // v0; the metering pipeline and the provider server log are the
      // authoritative record of a tool invocation.
    }

    // If the model produced no text (e.g. failure before any delta),
    // still emit the (possibly empty) response artifact so clients see
    // a well-formed artifact stream.
    if (!emittedText && result.state === 'completed') {
      yield this.artifactEvent(taskId, contextId, result.text, false, true);
      emittedText = true;
    } else if (emittedText && result.state === 'completed') {
      // Close the appended artifact.
      yield this.artifactEvent(taskId, contextId, '', true, true);
    }

    const finalTask = await this.finalize(taskId, result);
    yield this.statusEvent(taskId, contextId, finalTask.status, true);
  }

  // ── tasks/get ───────────────────────────────────────────────

  async tasksGet(params: TaskQueryParams, principal: Principal): Promise<Task> {
    const id = this.requireTaskId(params?.id);
    const record = await this.requireTask(id);
    await this.authorizer.authorizeProject(principal, record.projectName);
    return this.applyHistoryLength(record.task, params.historyLength);
  }

  // ── tasks/cancel ────────────────────────────────────────────

  async tasksCancel(params: TaskIdParams, principal: Principal): Promise<Task> {
    const id = this.requireTaskId(params?.id);
    const record = await this.requireTask(id);
    await this.authorizer.authorizeProject(principal, record.projectName);

    if (isTerminal(record.task.status.state)) {
      // A completed/failed/canceled task cannot be canceled.
      throw new JsonRpcError(
        A2A_TASK_NOT_CANCELABLE,
        `Task ${id} is in terminal state "${record.task.status.state}" and cannot be canceled`
      );
    }

    await this.taskStore.requestCancel(id);
    const updated = await this.transition(id, 'canceled');
    this.logger.info('a2a.task.canceled', { taskId: id, project: record.projectName });
    return updated.task;
  }

  // ── internals ───────────────────────────────────────────────

  private validateMessageParams(
    params: MessageSendParams
  ): { userText: string; projectName: string; contextId: string; userMessage: Message } {
    const message = params?.message;
    if (!message || message.kind !== 'message' || !Array.isArray(message.parts)) {
      throw new JsonRpcError(JSONRPC_INVALID_PARAMS, 'params.message must be an A2A Message object');
    }
    const userText = message.parts
      .filter((p): p is { kind: 'text'; text: string } => p.kind === 'text')
      .map((p) => p.text)
      .join(' ')
      .trim();
    if (!userText) {
      throw new JsonRpcError(
        JSONRPC_INVALID_PARAMS,
        'params.message.parts must include at least one non-empty text part'
      );
    }

    const projectName = this.extractProjectName(params);
    if (!projectName) {
      throw new JsonRpcError(
        JSONRPC_INVALID_PARAMS,
        'params.message.metadata.projectName is required (the project the task runs against)'
      );
    }
    // NOTE: project authorization (403) is NOT decided here — the caller
    // awaits this.authorizer.authorizeProject(principal, projectName)
    // right after validation. Keeps identity, param-shape, and the authz
    // decision as distinct concerns.
    const contextId = message.contextId?.trim() || this.generateId();
    return { userText, projectName, contextId, userMessage: message };
  }

  private extractProjectName(params: MessageSendParams): string | undefined {
    const fromMessage = params.message?.metadata?.projectName;
    if (typeof fromMessage === 'string' && fromMessage.trim()) return fromMessage.trim();
    const fromParams = params.metadata?.projectName;
    if (typeof fromParams === 'string' && fromParams.trim()) return fromParams.trim();
    return undefined;
  }

  private async createTaskRecord(
    parsed: { contextId: string; userMessage: Message; projectName: string },
    principal: Principal
  ): Promise<TaskRecord> {
    const taskId = this.generateId();
    const userMessage: Message = { ...parsed.userMessage, taskId, contextId: parsed.contextId };
    const task: Task = {
      kind: 'task',
      id: taskId,
      contextId: parsed.contextId,
      status: { state: 'submitted', timestamp: this.now() },
      history: [userMessage],
      metadata: { projectName: parsed.projectName },
    };
    const record: TaskRecord = {
      task,
      projectName: parsed.projectName,
      subject: principal.subject,
      cancelRequested: false,
    };
    await this.taskStore.create(record);
    this.logger.info('a2a.task.created', {
      taskId,
      contextId: parsed.contextId,
      project: parsed.projectName,
      subject: principal.subject,
    });
    return record;
  }

  private async transition(taskId: string, state: TaskState, message?: Message): Promise<TaskRecord> {
    const record = await this.taskStore.update(taskId, (r) => {
      r.task.status = { state, message, timestamp: this.now() };
    });
    if (!record) throw new JsonRpcError(A2A_TASK_NOT_FOUND, `Task ${taskId} not found`);
    return record;
  }

  private async finalize(taskId: string, result: AgentResult): Promise<Task> {
    const record = await this.taskStore.get(taskId);
    if (!record) throw new JsonRpcError(A2A_TASK_NOT_FOUND, `Task ${taskId} not found`);
    const { contextId } = record.task;

    let status: TaskStatus;
    let artifacts: Artifact[] | undefined;

    if (result.state === 'completed') {
      const agentMessage = this.agentMessage(result.text, taskId, contextId);
      status = { state: 'completed', message: agentMessage, timestamp: this.now() };
      if (result.text) artifacts = [this.responseArtifact(result.text)];
    } else if (result.state === 'canceled') {
      status = { state: 'canceled', timestamp: this.now() };
    } else {
      const errorMessage = this.agentMessage(
        result.error ? `Agent run failed: ${result.error}` : 'Agent run failed',
        taskId,
        contextId
      );
      status = { state: 'failed', message: errorMessage, timestamp: this.now() };
    }

    const updated = await this.taskStore.update(taskId, (r) => {
      r.task.status = status;
      if (artifacts) r.task.artifacts = artifacts;
      if (status.message) r.task.history = [...(r.task.history ?? []), status.message];
    });
    this.logger.info('a2a.task.finalized', {
      taskId,
      project: record.projectName,
      state: status.state,
      tokenEvents: result.usage.tokenEventCount,
      toolInvocationEvents: result.usage.toolInvocationEventCount,
      usageEmitted: result.usage.emitted,
    });
    return updated!.task;
  }

  private agentMessage(text: string, taskId: string, contextId: string): Message {
    return {
      kind: 'message',
      role: 'agent',
      parts: [{ kind: 'text', text }],
      messageId: this.generateId(),
      taskId,
      contextId,
    };
  }

  private responseArtifact(text: string): Artifact {
    return {
      artifactId: RESPONSE_ARTIFACT_ID,
      name: RESPONSE_ARTIFACT_NAME,
      parts: [{ kind: 'text', text }],
    };
  }

  private statusEvent(
    taskId: string,
    contextId: string,
    status: TaskStatus,
    final: boolean
  ): TaskStatusUpdateEvent {
    return { kind: 'status-update', taskId, contextId, status, final };
  }

  private artifactEvent(
    taskId: string,
    contextId: string,
    text: string,
    append: boolean,
    lastChunk: boolean
  ): StreamEvent {
    return {
      kind: 'artifact-update',
      taskId,
      contextId,
      artifact: this.responseArtifact(text),
      append,
      lastChunk,
    };
  }

  private applyHistoryLength(task: Task, historyLength?: number): Task {
    if (typeof historyLength !== 'number' || historyLength < 0 || !task.history) return task;
    return { ...task, history: task.history.slice(-historyLength) };
  }

  private requireTaskId(id: unknown): string {
    if (typeof id !== 'string' || !id.trim()) {
      throw new JsonRpcError(JSONRPC_INVALID_PARAMS, 'params.id (task id) is required');
    }
    return id;
  }

  private async requireTask(id: string): Promise<TaskRecord> {
    const record = await this.taskStore.get(id);
    if (!record) throw new JsonRpcError(A2A_TASK_NOT_FOUND, `Task ${id} not found`);
    return record;
  }
}

/** Exported for tests that want to assert cancelability rules directly. */
export { isCancelable };
