// The AdaClient interface mirrors adad's /v1 (internal/api). The React app talks to adad only
// through it; the adapters under ./adapters know how (decisions log, 2026-10-14, #5). The types
// mirror the API's JSON field for field.

export type TaskState =
  | "draft"
  | "ready"
  | "preparing"
  | "running"
  | "verifying"
  | "awaiting_review"
  | "accepted"
  | "refinement"
  | "failed"
  | "cancel_requested"
  | "canceled"
  | "interrupted"
  | "recovery_required";

export interface Task {
  id: string;
  project: string;
  created_by: string;
  objective: string;
  criteria: string;
  state: TaskState;
  revision: number;
  created: string;
  updated: string;
  attempt: TaskAttempt | null;
  // The snapshot the task's workspace is built from; absent for a task without one.
  snapshot?: string;
  // The provider configuration and hooks the snapshot puts in the worktree; null without one.
  provider_configuration?: ProviderConfiguration | null;
  checks?: CheckDefinition[];
}

export interface TaskAttempt {
  id: string;
  state: string;
  queue_position?: number;
  // The execution policy the attempt was started with; model and effort are "" when left to the CLI.
  backend?: string;
  model?: string;
  effort?: string;
  mode?: string;
  // What the attempt added, altered or removed of the provider configuration; null until assessed.
  provider_changes?: ProviderChanges | null;
  provider_flagged?: boolean;
  tested_commit?: string;
  checks?: CheckResult[];
  checks_failed?: boolean;
}

export interface ProviderConfiguration {
  paths: string[];
  count: number;
}

export interface ProviderChanges {
  changes: { path: string; change: string }[];
  truncated: boolean;
}

// A declared check: argv run in a fresh worktree, on the host, outside any sandbox (M4.2).
export interface CheckDefinition {
  name?: string;
  argv: string[];
  workdir?: string;
  timeout?: string;
  max_output_bytes?: number;
  env?: string[];
}

export type CheckState = "running" | "passed" | "failed" | "timed_out" | "killed" | "not_run" | "interrupted" | "changed_tree";

export interface CheckBaseline {
  id: string;
  snapshot?: string;
  state: CheckState;
  exit_code?: number;
  tested_commit: string;
  output_size: number;
  truncated: boolean;
  duration_ms: number;
}

export interface CheckResult {
  id: string;
  ord: number;
  name?: string;
  definition: CheckDefinition;
  state: CheckState;
  exit_code?: number;
  tested_commit?: string;
  bound_commit?: string;
  invalidated: boolean;
  output_size: number;
  truncated: boolean;
  baseline?: CheckBaseline;
  new_failure: boolean;
  baseline_failure: boolean;
  env_keys: string[];
  duration_ms: number;
}

// A check's log as read through the bridge: at most the bridge's cap of it, as text.
export interface CheckLog {
  text: string;
  // adad cut the log at the check's output cap (X-Ada-Truncated).
  truncated: boolean;
  // The bridge read only the first part of a longer log.
  partial: boolean;
}

// POST /v1/tasks/{id}/start answers with the task and the attempt it created.
export interface StartedTask extends Task {
  attempt_id: string;
}

export interface NewTask {
  project: string;
  objective: string;
  criteria?: string;
  snapshot?: string;
  checks?: CheckDefinition[];
}

export interface StopResult {
  running: number;
  queued: number;
  reason: string;
}

// The execution policy body of POST /v1/tasks/{id}/start.
export interface ExecutionPolicy {
  backend: string;
  model: string;
  effort: string;
  mode: "readonly" | "auto" | "yolo";
  // Caps passed to the engine when set: a Go duration, and a turn count where the backend offers one.
  timeout?: string;
  max_turns?: number;
}

// The capability matrix (GET /v1/capabilities, internal/engine): per backend and mode, what each
// row's cell claims; checks are unsandboxed.
export type CellStatus = "enforced" | "requested" | "unsupported" | "unverified" | "unsandboxed";
export interface CapabilityCell {
  status: CellStatus;
  by?: string;
  test?: string;
  detail: string;
  recorded?: CellStatus;
}
export interface CapabilityMode {
  mode: string;
  offered: boolean;
  reason?: string;
  cells?: Record<string, CapabilityCell>;
}
export interface CapabilityBackend {
  backend: string;
  cli_version: string;
  observed: string;
  installed?: { available: boolean; version: string; reason?: string };
  efforts: string[];
  options: string[];
  models: string[];
  modes: CapabilityMode[];
}
export interface Capabilities {
  engine: string;
  recorded: string;
  checks: CapabilityCell;
  backends: CapabilityBackend[];
}

// A folder the owner chose in the desktop's own dialog. The handle stands for the path, which
// stays in main: the renderer shows the path and never sends one (decisions log, 2026-10-20).
export interface ChosenFolder {
  handle: string;
  path: string;
}

export interface Blocker {
  code: string;
  paths: string[];
  count: number;
}

export interface Inspection {
  identity: { path: string; head: string; branch: string; detached: boolean };
  status: {
    staged: { path: string; status: string }[];
    unstaged: { path: string; status: string }[];
    untracked: string[];
    unmerged: string[];
  };
  history: { commit: string; author: string; date: string; subject: string }[];
  fingerprint: string;
  blockers: Blocker[];
  findings: { code: string; paths: string[]; count: number }[];
}

export interface Project {
  id: string;
  name: string;
  role: string;
}

export interface SnapshotSelection {
  untracked: string[];
  include_sensitive: string[];
  // Carry the staged and unstaged changes (true when absent); false takes the base commit and the
  // chosen untracked files only.
  include_changes?: boolean;
}

export interface Snapshot {
  id: string;
  project: string;
  base_commit: string;
  untracked: { path: string }[];
  provider_configuration: ProviderConfiguration | null;
}

// GET /v1/tasks/{id}/changes (M4.3). A path is git's bytes: the string when they are UTF-8, and
// also base64 in *_raw when they are not.
export interface ChangedFile {
  path: string;
  path_raw?: string;
  old_path?: string;
  old_path_raw?: string;
  status: string;
  similarity?: number;
  old_mode: string;
  new_mode: string;
  binary: boolean;
  added: number;
  deleted: number;
  patch: string;
  patch_raw?: string;
  patch_truncated: boolean;
  provider: boolean;
}
export interface Changes {
  task: string;
  attempt: string;
  base: string;
  commit: string;
  tip: string;
  stale: boolean;
  generation: number;
  files: ChangedFile[];
  total: number;
  files_truncated: boolean;
  bytes_truncated: boolean;
  total_capped: boolean;
  provider_changes: ProviderChanges | null;
  provider_flagged: boolean;
  checks: CheckResult[];
  // The scan of commit for credentials; null when the attempt was not scanned.
  secrets: SecretScan | null;
}

// A line the agent added that looks like a credential: where and which rule, never the value.
// Line 0 is the whole file (a key file such as .env).
export interface SecretFinding {
  id: string;
  path: string;
  path_raw?: string;
  line: number;
  rule: string;
}
export interface SecretScan {
  commit: string;
  rules_version: string;
  findings: SecretFinding[];
  findings_capped: boolean;
  not_scanned: { path: string; path_raw?: string; reason: string }[];
  unlisted: number;
  capped: boolean;
  failed: boolean;
}
// The answer to one open finding at accept: a test value (allow-listed, with its reason) or a
// real secret (the user rotates it; export refuses the branch while it holds the secret).
export interface SecretAck {
  finding: string;
  kind: "test" | "real";
  reason?: string;
}

// GET /v1/tasks/{id}/files: a directory of the agent-changes commit, or one file of it.
export interface FileEntry {
  name: string;
  name_raw?: string;
  path: string;
  path_raw?: string;
  type: string;
  mode: string;
  size: number;
  submodule?: string;
}
export interface FileView {
  task: string;
  commit: string;
  path: string;
  type: string;
  mode: string;
  entries?: FileEntry[];
  entries_truncated?: boolean;
  size: number;
  encoding?: string;
  content?: string;
  truncated: boolean;
  target?: string;
  target_raw?: string;
  submodule?: string;
}

export interface Finding {
  id: string;
  attempt: string;
  author: string;
  body: string;
  created: string;
}

export interface Accepted {
  task: Task;
  review: { id: string; attempt: string; reviewer: string; decision: string; commit: string; generation: number; secrets?: SecretAck[]; created: string };
}

export interface RefineRequest extends ExecutionPolicy {
  acknowledge_provider_changes: boolean;
}

export interface Refined extends StartedTask {
  findings: string[];
}

// One event of an attempt as the stream form carries it: id: seq, event: kind, data: payload.
export interface AttemptEvent {
  seq: number;
  kind: string;
  payload: unknown;
}

// What a subscription delivers. The renderer keeps no cursor: a reset means the server answered
// cursor_ahead and the stream was reopened from 0, so the view discards what it holds.
export type StreamMessage =
  | { type: "event"; event: AttemptEvent }
  | { type: "reset"; last_sequence: number }
  | { type: "status"; status: "connected" | "reconnecting" | "unreachable"; code?: string };

// The owner's account management (M4b.2). adad serves these on the local channel only, to the
// owner: the desktop reaches them, the browser never does (the network channel answers local_only).
export interface Account {
  id: string;
  name: string;
  owner: boolean;
  disabled: boolean;
  password_allowed: boolean;
  // How many credentials and live sessions the account holds.
  credentials: number;
  sessions: number;
  created: string;
}
export interface NewAccount {
  name: string;
  password_allowed: boolean;
}
export interface AccountChange {
  disabled?: boolean;
  password_allowed?: boolean;
}
// A single-use enrollment code, shown once; it expires 24 hours after issue.
export interface EnrollmentCode {
  code: string;
  expires: string;
}
export interface CreatedAccount extends EnrollmentCode {
  account: Account;
}
// A signed-in browser: never its token, only the id shown to the owner.
export interface AccountSession {
  id: string;
  account: string;
  created: string;
  last_used: string;
  address: string;
}

// The owner's network listener (M4b.3), on the local channel only, like the accounts. The paths
// of a certificate's files stay in the desktop's main process: the page never sees them.
export type ListenerReason = "" | "disabled" | "not_configured" | "certificate_missing" | "certificate_invalid" | "certificate_expired" | "certificate_name" | "listen_failed";
export interface ListenerCertificate {
  // The leaf's DNS names.
  names: string[];
  not_before: string;
  not_after: string;
  // The SHA-256 of the leaf's DER, in hex.
  sha256: string;
}
export interface ListenerStatus {
  enabled: boolean;
  // "" while unset.
  address: string;
  name: string;
  origin: string;
  state: "serving" | "off";
  reason: ListenerReason;
  // A sentence for the owner: where it serves, or what is wrong and what to do.
  message: string;
  // The installed pair, valid or not; null when none is installed.
  certificate: ListenerCertificate | null;
}
export interface ListenerSetting {
  enabled: boolean;
  address: string;
  name: string;
}

// The shell the app runs in: the desktop's preload bridge, or a browser on the network listener.
export type Channel = "desktop" | "network";

// GET /v1/projects: every project the user is a member of, those with no task yet included.
export interface ProjectSummary {
  id: string;
  name: string;
  role: string;
  created: string;
}

// The signed-in account (GET /v1/account), as sign-in and enrollment also answer it.
export interface SignedInAccount {
  id: string;
  name: string;
  owner: boolean;
  password_allowed: boolean;
}
// One of the signed-in account's own sessions; current is the one this browser holds.
export interface OwnSession {
  id: string;
  created: string;
  last_used: string;
  address: string;
  current: boolean;
}
export interface OwnCredential {
  id: string;
  kind: "passkey" | "password" | string;
  name: string;
  created: string;
  last_used: string;
}
// POST /v1/enrollment/options: the passkey ceremony for the code's account, and that account,
// whose password_allowed says whether a password may be enrolled instead.
export interface EnrollmentStart {
  ceremony: string;
  options: unknown;
  account: SignedInAccount;
}

// The body of every refused request: adad's error code and message.
export interface ErrorBody {
  error: string;
  message: string;
  request_id?: string;
}

// The parts of a refusal's body some answers carry beside the code: an import's blockers, the
// project already holding a repository, a snapshot's refused path, a refine's provider changes.
export interface ErrorDetails {
  blockers?: Blocker[];
  project?: string;
  tasks?: { id: string; objective: string; state: string }[];
  path?: string;
  provider_changes?: ProviderChanges;
  findings?: SecretFinding[];
  // The JSON event form's cursor_ahead names the attempt's last recorded sequence.
  last_sequence?: number;
}

// adad's codes (internal/api writeError), plus the client's own for a coordinator it cannot
// reach, a page without the bridge, a call the bridge refused as malformed, and an answer the
// bridge could not read (too large, cut short, or not the JSON the call needs).
export type ErrorCode =
  | "bad_request"
  | "too_large"
  | "not_found"
  | "method_not_allowed"
  | "stale_revision"
  | "invalid_transition"
  | "idempotency_conflict"
  | "engine_unavailable"
  | "experience_unavailable"
  | "internal"
  | "cursor_ahead"
  | "too_many_streams"
  | "shutting_down"
  | "queue_full"
  | "account_in_use"
  | "owner_account"
  | "local_only"
  | "name_taken"
  | "code_shown"
  | "address_invalid"
  | "name_invalid"
  | "certificate_invalid"
  | "certificate_expired"
  | "certificate_name"
  | "listener_unavailable"
  | "unauthenticated"
  | "not_on_network"
  | "login_failed"
  | "code_invalid"
  | "password_not_allowed"
  | "network_only"
  | "passkey_unavailable"
  | "coordinator_unreachable"
  | "bridge_unavailable"
  | "malformed_call"
  | "bad_response";

export class AdaError extends Error {
  readonly code: ErrorCode | string;
  readonly status: number;
  // The request id adad logs with an internal error, so the two can be matched.
  readonly requestId: string | undefined;
  readonly details: ErrorDetails | undefined;

  constructor(code: ErrorCode | string, status: number, message: string, requestId?: string, details?: ErrorDetails) {
    super(message);
    this.name = "AdaError";
    this.code = code;
    this.status = status;
    this.requestId = requestId;
    this.details = details;
  }
}

export type Experience = { version: 1; enabled: false } | {
  version: 1; enabled: true; level: number; xp: number; week_start: string;
  week: { delivery: number; practice: number }; streak: number | null; days: boolean[]; badges: string[];
  exercise: { kind: string; source: string; material?: string; answered: boolean } | null;
  familiarity: { known: ExperienceArea[]; unseen: ExperienceArea[] } | null;
};
export interface ExperienceArea { repo: string; area: string; count: number }

export interface AdaClient {
  experience(): Promise<Experience>;
  createTask(input: NewTask): Promise<Task>;
  listTasks(): Promise<Task[]>;
  getTask(id: string): Promise<Task>;
  ready(id: string, revision: number): Promise<Task>;
  start(id: string, revision: number, policy: ExecutionPolicy): Promise<StartedTask>;
  cancel(id: string, revision: number): Promise<Task>;
  subscribeEvents(attemptId: string, onMessage: (message: StreamMessage) => void): () => void;
  stopCoordinator(): Promise<StopResult>;
  finishQuit(): Promise<void>;
  onQuitRequested(listener: () => void): () => void;
  capabilities(): Promise<Capabilities>;
  // Opens the desktop's folder dialog; null when the owner closed it without choosing.
  chooseFolder(): Promise<ChosenFolder | null>;
  inspectFolder(handle: string): Promise<Inspection>;
  importFolder(handle: string, name: string): Promise<Project>;
  snapshot(handle: string, selection: SnapshotSelection): Promise<Snapshot>;
  changes(taskId: string): Promise<Changes>;
  // A path inside the task's agent-changes commit, "" for its root; never a host path.
  files(taskId: string, path: string): Promise<FileView>;
  checks(taskId: string): Promise<CheckResult[]>;
  checkLog(taskId: string, checkId: string): Promise<CheckLog>;
  findings(taskId: string): Promise<Finding[]>;
  addFinding(taskId: string, body: string): Promise<Finding>;
  accept(taskId: string, revision: number, commit: string, generation: number, secrets: SecretAck[]): Promise<Accepted>;
  refine(taskId: string, revision: number, request: RefineRequest): Promise<Refined>;
  listAccounts(): Promise<Account[]>;
  createAccount(input: NewAccount): Promise<CreatedAccount>;
  updateAccount(id: string, change: AccountChange): Promise<Account>;
  removeAccount(id: string): Promise<void>;
  issueEnrollmentCode(id: string): Promise<EnrollmentCode>;
  recoverAccount(id: string): Promise<EnrollmentCode>;
  listSessions(): Promise<AccountSession[]>;
  revokeSession(id: string): Promise<void>;
  listener(): Promise<ListenerStatus>;
  setListener(setting: ListenerSetting): Promise<ListenerStatus>;
  // Opens the desktop's two file dialogs, certificate then key; null when the owner cancelled one.
  installListenerCertificate(): Promise<ListenerStatus | null>;
  // Which shell this is; the browser offers no owner-channel or desktop-only control.
  readonly channel: Channel;
  projects(): Promise<ProjectSummary[]>;
  // The network channel's own routes: the desktop answers them with network_only.
  account(): Promise<SignedInAccount>;
  ownSessions(): Promise<OwnSession[]>;
  revokeOwnSession(id: string): Promise<void>;
  ownCredentials(): Promise<OwnCredential[]>;
  removeOwnCredential(id: string): Promise<void>;
  logout(): Promise<void>;
  // Called when a signed-in request finds the session gone (401); returns the unsubscribe.
  onSessionEnded(listener: () => void): () => void;
  // Asks for the options, has the browser's authenticator answer them, and sends the assertion.
  passkeyLogin(): Promise<SignedInAccount>;
  passwordLogin(account: string, password: string): Promise<SignedInAccount>;
  enrollmentOptions(code: string): Promise<EnrollmentStart>;
  // Has the authenticator create a passkey for start's ceremony and enrolls it under name.
  enrollPasskey(code: string, start: EnrollmentStart, name: string): Promise<SignedInAccount>;
  enrollPassword(code: string, password: string): Promise<SignedInAccount>;
}

// The preload bridge's shape, the `ada` global the desktop's preload exposes: the same methods,
// answering with plain data so a refusal crosses the context bridge intact.
export type BridgeResult<T> =
  | { ok: true; value: T }
  | { ok: false; error: { code: string; status: number; message: string; request_id?: string; details?: ErrorDetails } };

export interface BridgeApi {
  experience(): Promise<BridgeResult<Experience>>;
  createTask(input: NewTask): Promise<BridgeResult<Task>>;
  listTasks(): Promise<BridgeResult<Task[]>>;
  getTask(id: string): Promise<BridgeResult<Task>>;
  ready(id: string, revision: number): Promise<BridgeResult<Task>>;
  start(id: string, revision: number, policy: ExecutionPolicy): Promise<BridgeResult<StartedTask>>;
  cancel(id: string, revision: number): Promise<BridgeResult<Task>>;
  subscribeEvents(attemptId: string, onMessage: (message: StreamMessage) => void): () => void;
  stopCoordinator(): Promise<BridgeResult<StopResult>>;
  finishQuit(): Promise<BridgeResult<null>>;
  onQuitRequested(listener: () => void): () => void;
  capabilities(): Promise<BridgeResult<Capabilities>>;
  chooseFolder(): Promise<BridgeResult<ChosenFolder | null>>;
  inspectFolder(handle: string): Promise<BridgeResult<Inspection>>;
  importFolder(handle: string, name: string): Promise<BridgeResult<Project>>;
  snapshot(handle: string, selection: SnapshotSelection): Promise<BridgeResult<Snapshot>>;
  changes(taskId: string): Promise<BridgeResult<Changes>>;
  files(taskId: string, path: string): Promise<BridgeResult<FileView>>;
  checks(taskId: string): Promise<BridgeResult<CheckResult[]>>;
  checkLog(taskId: string, checkId: string): Promise<BridgeResult<CheckLog>>;
  findings(taskId: string): Promise<BridgeResult<Finding[]>>;
  addFinding(taskId: string, body: string): Promise<BridgeResult<Finding>>;
  accept(taskId: string, revision: number, commit: string, generation: number, secrets: SecretAck[]): Promise<BridgeResult<Accepted>>;
  refine(taskId: string, revision: number, request: RefineRequest): Promise<BridgeResult<Refined>>;
  listAccounts(): Promise<BridgeResult<Account[]>>;
  createAccount(input: NewAccount): Promise<BridgeResult<CreatedAccount>>;
  updateAccount(id: string, change: AccountChange): Promise<BridgeResult<Account>>;
  removeAccount(id: string): Promise<BridgeResult<null>>;
  issueEnrollmentCode(id: string): Promise<BridgeResult<EnrollmentCode>>;
  recoverAccount(id: string): Promise<BridgeResult<EnrollmentCode>>;
  listSessions(): Promise<BridgeResult<AccountSession[]>>;
  revokeSession(id: string): Promise<BridgeResult<null>>;
  listener(): Promise<BridgeResult<ListenerStatus>>;
  setListener(setting: ListenerSetting): Promise<BridgeResult<ListenerStatus>>;
  installListenerCertificate(): Promise<BridgeResult<ListenerStatus | null>>;
  listProjects(): Promise<BridgeResult<ProjectSummary[]>>;
}
