import type {
  AdaClient,
  Accepted,
  Account,
  AccountChange,
  AccountSession,
  Channel,
  CreatedAccount,
  EnrollmentCode,
  EnrollmentStart,
  OwnCredential,
  OwnSession,
  ProjectSummary,
  SignedInAccount,
  NewAccount,
  Capabilities,
  Changes,
  CheckLog,
  CheckResult,
  ChosenFolder,
  ExecutionPolicy,
  FileView,
  Finding,
  Inspection,
  ListenerCertificate,
  ListenerSetting,
  ListenerStatus,
  Member,
  MemberRole,
  Mode,
  NewTask,
  StopCounts,
  Project,
  RefineRequest,
  SecretAck,
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
  accountList: Account[] = [makeAccount({ id: "local", name: "owner", owner: true, credentials: 1, sessions: 1 })];
  sessionList: AccountSession[] = [];
  // The code each issue, create or recover hands out, in turn.
  codes = 0;
  listenerStatus: ListenerStatus = listenerStatus({ enabled: false, address: "", name: "" }, null);
  // The pair the certificate dialogs choose; null as if the owner cancelled them.
  chosenCertificate: ListenerCertificate | null = makeCertificate();

  channel: Channel = "desktop";
  projectList: ProjectSummary[] = [];
  // Each project's members, by project id.
  memberList: Record<string, Member[]> = {};
  stopCountList: StopCounts = { running: 1, queued: 1, others_running: 0, others_queued: 0 };
  // The browser's signed-in account; null answers 401 as adad does without a session.
  signedIn: SignedInAccount | null = null;
  // The accounts the browser may sign in or enroll as: by name, and by enrollment code.
  passwords: Record<string, { account: SignedInAccount; password: string }> = {};
  enrollCodes: Record<string, SignedInAccount> = {};
  // The account a passkey assertion signs in as; null when the authenticator has none.
  passkeyAccount: SignedInAccount | null = null;
  ownSessionList: OwnSession[] = [];
  credentialList: OwnCredential[] = [];

  constructor(tasks: Task[] = [], channel: Channel = "desktop") {
    this.tasks = tasks;
    this.channel = channel;
  }

  async projects(): Promise<ProjectSummary[]> {
    this.check("projects");
    return this.projectList;
  }
  private session(): SignedInAccount {
    if (this.signedIn === null) throw new AdaError("unauthenticated", 401, "sign in first");
    return this.signedIn;
  }
  async account(): Promise<SignedInAccount> {
    this.check("account");
    return this.session();
  }
  async ownSessions(): Promise<OwnSession[]> {
    this.check("ownSessions");
    this.session();
    return this.ownSessionList;
  }
  async revokeOwnSession(id: string): Promise<void> {
    this.check("revokeOwnSession", id);
    this.session();
    const current = this.ownSessionList.find((s) => s.id === id)?.current;
    this.ownSessionList = this.ownSessionList.filter((s) => s.id !== id);
    if (current) this.signedIn = null;
  }
  async ownCredentials(): Promise<OwnCredential[]> {
    this.check("ownCredentials");
    this.session();
    return this.credentialList;
  }
  async removeOwnCredential(id: string): Promise<void> {
    this.check("removeOwnCredential", id);
    this.session();
    if (this.credentialList.length === 1) throw new AdaError("last_credential", 409, "the account's last credential cannot be removed");
    this.credentialList = this.credentialList.filter((c) => c.id !== id);
    // As adad does, a credential change ends every session of the account.
    this.ownSessionList = [];
    this.signedIn = null;
  }
  sessionListeners = new Set<() => void>();
  onSessionEnded(listener: () => void): () => void {
    this.sessionListeners.add(listener);
    return () => this.sessionListeners.delete(listener);
  }
  async logout(): Promise<void> {
    this.check("logout");
    this.session();
    this.signedIn = null;
  }
  async passkeyLogin(): Promise<SignedInAccount> {
    this.check("passkeyLogin");
    if (this.passkeyAccount === null) throw new AdaError("login_failed", 401, "sign-in failed");
    this.signedIn = this.passkeyAccount;
    return this.signedIn;
  }
  async passwordLogin(account: string, password: string): Promise<SignedInAccount> {
    this.check("passwordLogin", account, password);
    const entry = this.passwords[account];
    if (!entry || entry.password !== password || !entry.account.password_allowed) throw new AdaError("login_failed", 401, "sign-in failed");
    this.signedIn = entry.account;
    return this.signedIn;
  }
  async enrollmentOptions(code: string): Promise<EnrollmentStart> {
    this.check("enrollmentOptions", code);
    const account = this.enrollCodes[code];
    if (!account) throw new AdaError("code_invalid", 400, "the enrollment code is unknown, used or expired");
    return { ceremony: `ceremony-${code}`, options: {}, account };
  }
  async enrollPasskey(code: string, start: EnrollmentStart, name: string): Promise<SignedInAccount> {
    this.check("enrollPasskey", code, start.ceremony, name);
    return this.enrolled(code);
  }
  async enrollPassword(code: string, password: string): Promise<SignedInAccount> {
    this.check("enrollPassword", code, password);
    const account = this.enrollCodes[code];
    if (account && !account.password_allowed) throw new AdaError("password_not_allowed", 403, "the account does not allow a password");
    return this.enrolled(code);
  }
  private enrolled(code: string): SignedInAccount {
    const account = this.enrollCodes[code];
    if (!account) throw new AdaError("code_invalid", 400, "the enrollment code is unknown, used or expired");
    delete this.enrollCodes[code];
    this.signedIn = account;
    return account;
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
    // As adad does, the task now shows the new attempt with the policy it was started with.
    const attempt = { id: `attempt-${id}`, state: "pending", ...policy };
    const task = { ...this.update(id, revision, "preparing"), attempt };
    this.tasks = this.tasks.map((t) => (t.id === id ? task : t));
    return { ...task, attempt_id: attempt.id };
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
  async stopCounts(): Promise<StopCounts> {
    this.check("stopCounts");
    return this.stopCountList;
  }
  async stop(others: number) {
    this.check("stop", others);
    const counts = this.stopCountList;
    if (others !== counts.others_running + counts.others_queued) throw new AdaError("stop_count_changed", 409, "the count of other users' work changed", undefined, { ...counts });
    return { running: counts.running, queued: counts.queued, reason: "coordinator stopped by the owner" };
  }
  async finishQuit(): Promise<void> {
    this.check("finishQuit");
  }
  onQuitRequested(_listener: () => void): () => void {
    return () => {};
  }

  async experience(): Promise<import("../client/api").Experience> {
    this.check("experience");
    return { version: 1, enabled: false };
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
  async snapshotProject(projectId: string, selection: SnapshotSelection): Promise<Snapshot> {
    this.check("snapshotProject", projectId, selection);
    return { id: "snap1", project: projectId, base_commit: "b".repeat(40), untracked: [], provider_configuration: null };
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
  async accept(taskId: string, revision: number, commit: string, generation: number, secrets: SecretAck[]): Promise<Accepted> {
    this.check("accept", taskId, revision, commit, generation, secrets);
    const task = this.update(taskId, revision, "accepted");
    return { task, review: { id: "r1", attempt: "a1", reviewer: "local", decision: "accepted", commit, generation, secrets, created: "" } };
  }
  async refine(taskId: string, revision: number, request: RefineRequest): Promise<Refined> {
    this.check("refine", taskId, revision, request);
    const attempt = { id: `attempt-${taskId}-2`, state: "pending" };
    const task = { ...this.update(taskId, revision, "preparing"), attempt };
    this.tasks = this.tasks.map((t) => (t.id === taskId ? task : t));
    return { ...task, attempt_id: attempt.id, findings: this.findingList.map((f) => f.id) };
  }

  async decide(attemptId: string, decision: "allow" | "deny"): Promise<Task> {
    this.check("decide", attemptId, decision);
    const task = this.tasks.find((t) => t.attempt?.id === attemptId);
    if (!task?.attempt?.decision) throw new AdaError("invalid_transition", 409, "invalid transition");
    const decided = { ...task.attempt.decision, allowed: decision === "allow", denied: decision === "deny" };
    const updated: Task = { ...task, state: decision === "deny" ? "canceled" : task.state, revision: task.revision + 1, attempt: { ...task.attempt, state: decision === "deny" ? "canceled" : task.attempt.state, decision: decided } };
    this.tasks = this.tasks.map((t) => (t.id === task.id ? updated : t));
    return updated;
  }
  async members(projectId: string): Promise<Member[]> {
    this.check("members", projectId);
    return this.memberList[projectId] ?? [];
  }
  async setMember(projectId: string, user: string, role: MemberRole): Promise<Member[]> {
    this.check("setMember", projectId, user, role);
    const list = this.memberList[projectId] ?? [];
    const name = this.accountOf(user).name;
    this.memberList[projectId] = list.some((m) => m.user === user) ? list.map((m) => (m.user === user ? { ...m, role } : m)) : [...list, { user, name, role, added: "2026-11-25T00:00:00Z" }];
    return this.memberList[projectId];
  }
  async removeMember(projectId: string, user: string): Promise<Member[]> {
    this.check("removeMember", projectId, user);
    const list = this.memberList[projectId] ?? [];
    const owners = list.filter((m) => m.role === "owner");
    if (owners.length === 1 && owners[0]!.user === user) throw new AdaError("last_owner", 409, "the project's last owner cannot be removed");
    this.memberList[projectId] = list.filter((m) => m.user !== user);
    return this.memberList[projectId];
  }
  async setDefaultMode(projectId: string, mode: Mode): Promise<{ id: string; default_mode: Mode }> {
    this.check("setDefaultMode", projectId, mode);
    this.projectList = this.projectList.map((p) => (p.id === projectId ? { ...p, default_mode: mode } : p));
    return { id: projectId, default_mode: mode };
  }

  async listAccounts(): Promise<Account[]> {
    this.check("listAccounts");
    return this.accountList;
  }
  async createAccount(input: NewAccount): Promise<CreatedAccount> {
    this.check("createAccount", input);
    const account = makeAccount({ id: `u${this.accountList.length + 1}`, name: input.name, password_allowed: input.password_allowed });
    this.accountList = [...this.accountList, account];
    return { account, ...this.code() };
  }
  async updateAccount(id: string, change: AccountChange): Promise<Account> {
    this.check("updateAccount", id, change);
    const account = { ...this.accountOf(id), ...change };
    // As adad does, disabling ends the account's sessions.
    if (change.disabled) this.endSessions(id);
    this.accountList = this.accountList.map((a) => (a.id === id ? { ...account, sessions: change.disabled ? 0 : account.sessions } : a));
    return this.accountOf(id);
  }
  async removeAccount(id: string): Promise<void> {
    this.check("removeAccount", id);
    this.accountOf(id);
    this.endSessions(id);
    this.accountList = this.accountList.filter((a) => a.id !== id);
  }
  async issueEnrollmentCode(id: string): Promise<EnrollmentCode> {
    this.check("issueEnrollmentCode", id);
    this.accountOf(id);
    return this.code();
  }
  async recoverAccount(id: string): Promise<EnrollmentCode> {
    this.check("recoverAccount", id);
    this.endSessions(id);
    this.accountList = this.accountList.map((a) => (a.id === id ? { ...a, sessions: 0 } : a));
    return this.code();
  }
  async listSessions(): Promise<AccountSession[]> {
    this.check("listSessions");
    return this.sessionList;
  }
  async revokeSession(id: string): Promise<void> {
    this.check("revokeSession", id);
    const session = this.sessionList.find((s) => s.id === id);
    if (!session) throw new AdaError("not_found", 404, "no such session");
    this.sessionList = this.sessionList.filter((s) => s.id !== id);
    this.accountList = this.accountList.map((a) => (a.id === session.account ? { ...a, sessions: a.sessions - 1 } : a));
  }
  async listener(): Promise<ListenerStatus> {
    this.check("listener");
    return this.listenerStatus;
  }
  async setListener(setting: ListenerSetting): Promise<ListenerStatus> {
    this.check("setListener", setting);
    this.listenerStatus = listenerStatus(setting, this.listenerStatus.certificate);
    return this.listenerStatus;
  }
  async installListenerCertificate(): Promise<ListenerStatus | null> {
    this.check("installListenerCertificate");
    if (this.chosenCertificate === null) return null;
    this.listenerStatus = listenerStatus(this.listenerStatus, this.chosenCertificate);
    return this.listenerStatus;
  }

  private accountOf(id: string): Account {
    const account = this.accountList.find((a) => a.id === id);
    if (!account) throw new AdaError("not_found", 404, "no such account");
    return account;
  }
  private endSessions(account: string): void {
    this.sessionList = this.sessionList.filter((s) => s.account !== account);
  }
  private code(): EnrollmentCode {
    this.codes++;
    return { code: `ABCDE12345FGHJK6789${this.codes % 10}`, expires: "2026-11-24T10:00:00Z" };
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

export function makeAccount(fields: Partial<Account> & { id: string }): Account {
  return { name: fields.id, owner: false, disabled: false, password_allowed: false, credentials: 0, sessions: 0, created: "2026-11-01T00:00:00Z", ...fields };
}

export function makeCertificate(fields: Partial<ListenerCertificate> = {}): ListenerCertificate {
  return { names: ["home.adanext.si"], not_before: "2026-11-01T00:00:00Z", not_after: "2027-02-01T00:00:00Z", sha256: "ab".repeat(32), ...fields };
}

// The status adad reports for a setting and an installed pair, as far as the fake can tell.
export function listenerStatus(setting: ListenerSetting, certificate: ListenerCertificate | null): ListenerStatus {
  const port = setting.address.slice(setting.address.lastIndexOf(":") + 1);
  const origin = setting.name === "" ? "" : port === "443" ? `https://${setting.name}` : `https://${setting.name}:${port}`;
  const base = { enabled: setting.enabled, address: setting.address, name: setting.name, origin, certificate };
  if (!setting.enabled) return { ...base, state: "off", reason: "disabled", message: "The network listener is turned off." };
  if (setting.address === "" || setting.name === "") return { ...base, state: "off", reason: "not_configured", message: "Set the address and the name to turn the listener on." };
  if (certificate === null) return { ...base, state: "off", reason: "certificate_missing", message: "Install a certificate and its key to turn the listener on." };
  return { ...base, state: "serving", reason: "", message: "" };
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
