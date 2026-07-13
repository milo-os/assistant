// src/a2a/tasks.ts
//
// Task persistence seam. v0 ships an in-memory store; the interface is
// the extension point for a durable backend (see README "Follow-ups").
//
// A `TaskRecord` wraps the wire-shape `Task` with server-internal fields
// that must NOT leak onto the A2A wire: the owning project (for authz on
// tasks/get and tasks/cancel) and a cancellation flag the agent loop
// polls between steps.
import type { Task, TaskState } from './types';
import { isTerminal } from './types';

export interface TaskRecord {
  task: Task;
  /** Milo project the task runs against; used to authorize tasks/get & cancel. */
  projectName: string;
  /** Auth subject that created the task (for audit/debugging). */
  subject: string;
  /** Set by tasks/cancel; the agent loop polls this to stop early. */
  cancelRequested: boolean;
}

export interface TaskStore {
  create(record: TaskRecord): Promise<void>;
  get(id: string): Promise<TaskRecord | undefined>;
  /**
   * Apply `mutate` to the stored record and persist it. Returns the
   * updated record, or undefined if no task with that id exists.
   */
  update(id: string, mutate: (record: TaskRecord) => void): Promise<TaskRecord | undefined>;
  /** Flag the task for cancellation. Returns the record (post-flag) or undefined. */
  requestCancel(id: string): Promise<TaskRecord | undefined>;
  /** Synchronous peek used by the agent loop between steps. */
  isCancelRequested(id: string): boolean;
}

export class InMemoryTaskStore implements TaskStore {
  private readonly records = new Map<string, TaskRecord>();

  async create(record: TaskRecord): Promise<void> {
    this.records.set(record.task.id, record);
  }

  async get(id: string): Promise<TaskRecord | undefined> {
    return this.records.get(id);
  }

  async update(
    id: string,
    mutate: (record: TaskRecord) => void
  ): Promise<TaskRecord | undefined> {
    const record = this.records.get(id);
    if (!record) return undefined;
    mutate(record);
    return record;
  }

  async requestCancel(id: string): Promise<TaskRecord | undefined> {
    const record = this.records.get(id);
    if (!record) return undefined;
    record.cancelRequested = true;
    return record;
  }

  isCancelRequested(id: string): boolean {
    return this.records.get(id)?.cancelRequested ?? false;
  }
}

/** Convenience: is a task in a state that tasks/cancel may still act on? */
export function isCancelable(state: TaskState): boolean {
  return !isTerminal(state);
}
