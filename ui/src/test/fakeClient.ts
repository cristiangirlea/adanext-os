import type {
  AdaClient,
  Accepted,
  Capabilities,
  Changes,
  CheckLog,
  CheckResult,
  ChosenFolder,
  ExecutionPolicy,
  FileView,
  Finding,
  Inspection,
  NewTask,
  Project,
  RefineRequest,
  Refined,
  Snapshot,
  SnapshotSelection,
  StartedTask,
  StreamMessage,
  Task,
} from "../client/api";
import { AdaError } from "../client/api";

// A fake AdaClient for component tests: tasks are held in memory, every call is recorded, a
// subscription hands its callback to the test so it can push messages in the order it chooses.
// The workspace's reads answer from the fields below; a missing one answers as adad would.
export class FakeClient implements AdaClient {
  tasks: Task[] = [];
  calls: string[] = [];
  args: Record<string, unknown[][]> = {};
  listeners = new Map<string, (message: StreamMessage) => void>();
  unsubscribed: string[] = [];
  refuse: Partial<Record<keyof AdaClient, Error>> = {};
  // While hold is set, getTask answers with the task as it was asked for, once released.
  hold = false;
  inFlight = 0;
  maxInFlight = 0;
  private held: (() => void)[] = [];

  matrix: Capabilities = capabilities();
  folder: ChosenFolder | null = { handle: "h1", path: "C:\\work\\repo" };
  inspection: Inspection = inspection();
  project: Project = { id: "p-new", name: "repo", role: "owner" };
  changeSets: Record<string, Changes> = {};
  fileViews: Record<string, FileView> = {};
  checkResults: Record<string, CheckResult[]> = {};
  logs: Record<string, CheckLog> = {};
  findingList: Finding[] = [];

  constructor(tasks: Task[] = []) {
    this.tasks = tasks;
  }

  private check(method: keyof AdaClient, ...args: unknown[]): void {
    this.calls.push(method);
    (this.args[method] ??= []).push(args);
    const error = this.refuse[method];
    if (error) throw error;
  }

  async createTask(input: NewTask): Promise<Task> {
    this.check("createTask", input);
    const task = makeTask({ id: `t${this.tasks.length + 1}`, project: input.project, objective: input.objective, criteria: input.criteria ?? "", snapshot: input.snapshot, checks: input.checks ?? [] });
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
    this.check("start", id, revision, policy);
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
  async finishQuit(): Promise<void> {
    this.check("finishQuit");
  }
  onQuitRequested(): () => void {
    return () => {};
  }

  async capabilities(): Promise<Capabilities> {
    this.check("capabilities");
    return this.matrix;
  }
  async chooseFolder(): Promise<ChosenFolder | null> {
    this.check("chooseFolder");
    return this.folder;
  }
  async inspectFolder(handle: string): Promise<Inspection> {
    this.check("inspectFolder", handle);
    return this.inspection;
  }
  async importFolder(handle: string, name: string): Promise<Project> {
    this.check("importFolder", handle, name);
    return this.project;
  }
  async snapshot(handle: string, selection: SnapshotSelection): Promise<Snapshot> {
    this.check("snapshot", handle, selection);
    return { id: "snap1", project: this.project.id, base_commit: "b".repeat(40), untracked: selection.untracked.map((path) => ({ path })), provider_configuration: null };
  }
  async changes(taskId: string): Promise<Changes> {
    this.check("changes", taskId);
    const value = this.changeSets[taskId];
    if (!value) throw new AdaError("no_changes", 409, "the task has no agent changes yet");
    return value;
  }
  async files(taskId: string, path: string): Promise<FileView> {
    this.check("files", taskId, path);
    const value = this.fileViews[path];
    if (!value) throw new AdaError("no_changes", 409, "the task has no agent changes yet");
    return value;
  }
  async checks(taskId: string): Promise<CheckResult[]> {
    this.check("checks", taskId);
    return this.checkResults[taskId] ?? [];
  }
  async checkLog(taskId: string, checkId: string): Promise<CheckLog> {
    this.check("checkLog", taskId, checkId);
    return this.logs[checkId] ?? { text: "", truncated: false, partial: false };
  }
  async findings(taskId: string): Promise<Finding[]> {
    this.check("findings", taskId);
    return this.findingList;
  }
  async addFinding(taskId: string, body: string): Promise<Finding> {
    this.check("addFinding", taskId, body);
    const finding = { id: `f${this.findingList.length + 1}`, attempt: "a1", author: "local", body, created: "2026-10-28T00:00:00Z" };
    this.findingList = [...this.findingList, finding];
    return finding;
  }
  async accept(taskId: string, revision: number, commit: string, generation: number): Promise<Accepted> {
    this.check("accept", taskId, revision, commit, generation);
    const task = this.update(taskId, revision, "accepted");
    return { task, review: { id: "r1", attempt: "a1", reviewer: "local", decision: "accepted", commit, generation, created: "" } };
  }
  async refine(taskId: string, revision: number, request: RefineRequest): Promise<Refined> {
    this.check("refine", taskId, revision, request);
    const attempt = { id: `attempt-${taskId}-2`, state: "pending" };
    const task = { ...this.update(taskId, revision, "preparing"), attempt };
    this.tasks = this.tasks.map((t) => (t.id === taskId ? task : t));
    return { ...task, attempt_id: attempt.id, findings: this.findingList.map((f) => f.id) };
  }

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

// The pinned engine's matrix, cut down to what the UI reads: claude and codex, each with
// readonly, auto and yolo offered and ask refused.
export function capabilities(): Capabilities {
  const cell = (status: Capabilities["checks"]["status"], detail: string, by?: string) => ({ status, detail, by });
  const modes = (readOnly: Capabilities["checks"]) => [
    { mode: "readonly", offered: true, cells: { read_only: readOnly, workspace_writes_and_shell: cell("unsupported", "readonly asks for no writes"), network: cell("unsupported", "no network sandbox") } },
    { mode: "auto", offered: true, cells: { read_only: cell("unsupported", "auto lets the agent edit"), workspace_writes_and_shell: cell("requested", "edits are accepted"), network: cell("unsupported", "no network sandbox") } },
    { mode: "yolo", offered: true, cells: { read_only: cell("unsupported", "yolo lets the agent do anything"), workspace_writes_and_shell: cell("requested", "every tool runs without asking, as the user, on the host; nothing keeps it inside the worktree"), network: cell("unsupported", "no network sandbox") } },
    { mode: "ask", offered: false, reason: "mode ask needs approvals, which the pinned engine does not have (#7)" },
  ];
  return {
    engine: "8dd2659",
    recorded: "2026-10-25",
    checks: cell("unsandboxed", "checks run the repository's code as the user, on the host, with network", "adad"),
    backends: [
      { backend: "claude", cli_version: "2.1.284", observed: "2026-10-25", installed: { available: true, version: "2.1.284" }, efforts: ["low", "high"], options: ["model", "mode"], models: ["claude-opus-5-5"], modes: modes(cell("requested", "plan mode refuses edits", "claude --permission-mode plan")) },
      { backend: "codex", cli_version: "0.163.0-alpha.11", observed: "2026-10-25", efforts: ["low"], options: ["model", "mode"], models: ["gpt-6.1-sol", "gpt-6-luna"], modes: modes(cell("enforced", "codex's sandbox denied the write", "codex --sandbox read-only")) },
    ],
  };
}

export function inspection(): Inspection {
  return {
    identity: { path: "C:\\work\\repo", head: "c".repeat(40), branch: "main", detached: false },
    status: { staged: [{ path: "src/app.ts", status: "M" }], unstaged: [{ path: "README.md", status: "M" }], untracked: ["notes.txt", ".env", "keys/id_ed25519"], unmerged: [] },
    history: [{ commit: "c".repeat(40), author: "Owner", date: "2026-10-27", subject: "start" }],
    fingerprint: "f",
    blockers: [],
    findings: [{ code: "provider_configuration", paths: [".claude/settings.json"], count: 1 }],
  };
}
