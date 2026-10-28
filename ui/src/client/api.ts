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
  review: { id: string; attempt: string; reviewer: string; decision: string; commit: string; generation: number; created: string };
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
  | "internal"
  | "cursor_ahead"
  | "too_many_streams"
  | "shutting_down"
  | "queue_full"
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

export interface AdaClient {
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
  accept(taskId: string, revision: number, commit: string, generation: number): Promise<Accepted>;
  refine(taskId: string, revision: number, request: RefineRequest): Promise<Refined>;
}

// The preload bridge's shape, the `ada` global the desktop's preload exposes: the same methods,
// answering with plain data so a refusal crosses the context bridge intact.
export type BridgeResult<T> =
  | { ok: true; value: T }
  | { ok: false; error: { code: string; status: number; message: string; request_id?: string; details?: ErrorDetails } };

export interface BridgeApi {
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
  accept(taskId: string, revision: number, commit: string, generation: number): Promise<BridgeResult<Accepted>>;
  refine(taskId: string, revision: number, request: RefineRequest): Promise<BridgeResult<Refined>>;
}
