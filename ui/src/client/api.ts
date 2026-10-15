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
}

// POST /v1/tasks/{id}/start answers with the task and the attempt it created.
export interface StartedTask extends Task {
  attempt_id: string;
}

export interface NewTask {
  project: string;
  objective: string;
  criteria?: string;
}

// The execution policy body of POST /v1/tasks/{id}/start.
export interface ExecutionPolicy {
  backend: string;
  model: string;
  effort: string;
  mode: "readonly" | "auto" | "yolo";
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

// adad's codes (internal/api writeError), plus the client's own for a coordinator it cannot
// reach, a page without the bridge, and a call the bridge refused as malformed.
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
  | "malformed_call";

export class AdaError extends Error {
  readonly code: ErrorCode | string;
  readonly status: number;

  constructor(code: ErrorCode | string, status: number, message: string) {
    super(message);
    this.name = "AdaError";
    this.code = code;
    this.status = status;
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
  stopCoordinator(): Promise<void>;
}

// The preload bridge's shape, the `ada` global the desktop's preload exposes: the same methods,
// answering with plain data so a refusal crosses the context bridge intact.
export type BridgeResult<T> = { ok: true; value: T } | { ok: false; error: { code: string; status: number; message: string } };

export interface BridgeApi {
  createTask(input: NewTask): Promise<BridgeResult<Task>>;
  listTasks(): Promise<BridgeResult<Task[]>>;
  getTask(id: string): Promise<BridgeResult<Task>>;
  ready(id: string, revision: number): Promise<BridgeResult<Task>>;
  start(id: string, revision: number, policy: ExecutionPolicy): Promise<BridgeResult<StartedTask>>;
  cancel(id: string, revision: number): Promise<BridgeResult<Task>>;
  subscribeEvents(attemptId: string, onMessage: (message: StreamMessage) => void): () => void;
  stopCoordinator(): Promise<BridgeResult<null>>;
}
