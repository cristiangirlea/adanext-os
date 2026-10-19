import type { AdaClient, ExecutionPolicy, NewTask, StartedTask, StreamMessage, Task } from "../client/api";

// A fake AdaClient for component tests: tasks are held in memory, every call is recorded, a
// subscription hands its callback to the test so it can push messages in the order it chooses.
export class FakeClient implements AdaClient {
  tasks: Task[] = [];
  calls: string[] = [];
  listeners = new Map<string, (message: StreamMessage) => void>();
  unsubscribed: string[] = [];
  refuse: Partial<Record<keyof AdaClient, Error>> = {};
  // While hold is set, getTask answers with the task as it was asked for, once released.
  hold = false;
  inFlight = 0;
  maxInFlight = 0;
  private held: (() => void)[] = [];

  constructor(tasks: Task[] = []) {
    this.tasks = tasks;
  }

  private check(method: keyof AdaClient): void {
    this.calls.push(method);
    const error = this.refuse[method];
    if (error) throw error;
  }

  async createTask(input: NewTask): Promise<Task> {
    this.check("createTask");
    const task = makeTask({ id: `t${this.tasks.length + 1}`, project: input.project, objective: input.objective, criteria: input.criteria ?? "" });
    this.tasks = [...this.tasks, task];
    return task;
  }
  async listTasks(): Promise<Task[]> {
    this.check("listTasks");
    return this.tasks;
  }
  async getTask(id: string): Promise<Task> {
    this.check("getTask");
    this.calls.push(`getTask:${id}`);
    const task = this.find(id);
    this.inFlight++;
    this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
    try {
      if (this.hold) await new Promise<void>((resolve) => this.held.push(resolve));
      return task;
    } finally {
      this.inFlight--;
    }
  }
  // release answers every held read and says how many there were.
  release(): number {
    const held = this.held.splice(0);
    for (const resolve of held) resolve();
    return held.length;
  }
  async ready(id: string, revision: number): Promise<Task> {
    this.check("ready");
    return this.update(id, revision, "ready");
  }
  async start(id: string, revision: number, policy: ExecutionPolicy): Promise<StartedTask> {
    this.check("start");
    this.calls.push(`start:${policy.backend}:${policy.mode}`);
    return { ...this.update(id, revision, "preparing"), attempt_id: `attempt-${id}` };
  }
  async cancel(id: string, revision: number): Promise<Task> {
    this.check("cancel");
    return this.update(id, revision, "cancel_requested");
  }
  subscribeEvents(attemptId: string, onMessage: (message: StreamMessage) => void): () => void {
    this.calls.push(`subscribe:${attemptId}`);
    this.listeners.set(attemptId, onMessage);
    return () => {
      this.listeners.delete(attemptId);
      this.unsubscribed.push(attemptId);
    };
  }
  async stopCoordinator() {
    this.check("stopCoordinator");
    return { running: 1, queued: 1, reason: "coordinator stopped by the owner" };
  }
  async finishQuit(): Promise<void> { this.check("finishQuit"); }
  onQuitRequested(): () => void { return () => {}; }

  private find(id: string): Task {
    const task = this.tasks.find((t) => t.id === id);
    if (!task) throw new Error(`fake: no task ${id}`);
    return task;
  }
  private update(id: string, revision: number, state: Task["state"]): Task {
    const task = this.find(id);
    if (task.revision !== revision) throw new Error(`fake: stale revision ${revision} for ${id}`);
    const updated = { ...task, state, revision: revision + 1 };
    this.tasks = this.tasks.map((t) => (t.id === id ? updated : t));
    return updated;
  }
}

export function makeTask(fields: Partial<Task> & { id: string }): Task {
  return {
    project: "p1",
    created_by: "local",
    objective: `objective ${fields.id}`,
    criteria: "",
    state: "draft",
    revision: 0,
    created: "2026-10-15T00:00:00Z",
    updated: "2026-10-15T00:00:00Z",
    attempt: null,
    ...fields,
  };
}
