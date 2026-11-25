import { AdaError, type AdaClient, type ErrorDetails, type SignedInAccount, type StreamMessage } from "../api";
import { createPasskey, getPasskey } from "./webauthn";

// The browser adapter: same-origin fetch to adad's /v1 on the network listener, with the session
// cookie, and EventSource for an attempt's events (decisions log, 2026-10-14, #4 and #5). The
// owner's channel and the desktop's dialogs are not on the network: those methods answer
// local_only without a request. This module and ./webauthn are the only ones that may reach
// fetch, EventSource or navigator.credentials (ui/src/boundary.test.ts).

// The kinds an attempt's events carry (internal/api/doc.go): EventSource delivers a named event
// only to a listener for that name.
export const eventKinds = ["text", "thinking", "tool_call", "tool_result", "info", "error", "session", "engine_notice", "result", "executor_error", "recovery", "check_cleanup_refused", "cancel", "message"];

// What a check log read keeps, as the bridge does.
const checkLogLimit = 1 << 20;
const detailKeys = ["blockers", "project", "tasks", "path", "provider_changes", "findings", "last_sequence"] as const;
// Sign-in and enrollment answer 401 for a refused credential, not for a lost session.
const openRoutes = /^\/v1\/(session\/(passkey|password)|enrollment\/)/;

interface RecordedEvent {
  seq: number;
  kind: string;
  payload: unknown;
}

export interface HttpsOptions {
  fetch?: typeof fetch;
  EventSource?: typeof EventSource;
  // The first delay before a refused stream is opened again, doubling to maxDelayMs.
  initialDelayMs?: number;
  maxDelayMs?: number;
  schedule?: (fn: () => void, ms: number) => () => void;
}

interface Request {
  body?: unknown;
  // The expected task revision, sent as If-Match in the ETag's quoted form.
  revision?: number;
  idempotent?: boolean;
  // Sends {} where the route reads an empty JSON object as no input.
  emptyObject?: boolean;
}

function localOnly(): Promise<never> {
  return Promise.reject(new AdaError("local_only", 403, "only the owner's desktop can do this"));
}

function details(body: Record<string, unknown>): ErrorDetails | undefined {
  const out: Record<string, unknown> = {};
  for (const key of detailKeys) if (body[key] !== undefined) out[key] = body[key];
  return Object.keys(out).length ? (out as ErrorDetails) : undefined;
}

async function refusal(res: Response): Promise<AdaError> {
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    return new AdaError(`http_${res.status}`, res.status, `adad answered ${res.status}`);
  }
  if (body === null || typeof body !== "object" || Array.isArray(body)) return new AdaError(`http_${res.status}`, res.status, `adad answered ${res.status}`);
  const b = body as Record<string, unknown>;
  const code = typeof b.error === "string" ? b.error : `http_${res.status}`;
  const message = typeof b.message === "string" ? b.message : `adad answered ${res.status}`;
  return new AdaError(code, res.status, message, typeof b.request_id === "string" ? b.request_id : undefined, details(b));
}

const id = (value: string) => encodeURIComponent(value);

export function createHttpsClient(options: HttpsOptions = {}): AdaClient {
  const doFetch = options.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init));
  const Source = options.EventSource ?? (typeof EventSource === "undefined" ? undefined : EventSource);
  const initial = options.initialDelayMs ?? 500;
  const max = options.maxDelayMs ?? 10_000;
  const schedule =
    options.schedule ??
    ((fn: () => void, ms: number) => {
      const timer = setTimeout(fn, ms);
      return () => clearTimeout(timer);
    });

  // A 401 from a protected route means the session is gone; the browser shell reloads the page.
  const sessionListeners = new Set<() => void>();
  const sessionEnded = () => {
    for (const listener of [...sessionListeners]) listener();
  };
  const lostSession = (e: unknown) => e instanceof AdaError && e.status === 401;

  const send = async (method: string, path: string, request: Request = {}): Promise<Response> => {
    const headers: Record<string, string> = { Accept: "application/json" };
    let body: string | undefined;
    if (method !== "GET") {
      // The listener takes a change only as JSON, from this origin.
      headers["Content-Type"] = "application/json";
      body = request.body !== undefined ? JSON.stringify(request.body) : request.emptyObject ? "{}" : "";
    }
    if (request.revision !== undefined) headers["If-Match"] = `"${request.revision}"`;
    if (request.idempotent) headers["Idempotency-Key"] = crypto.randomUUID();
    let res: Response;
    try {
      res = await doFetch(path, { method, headers, body, credentials: "same-origin", cache: "no-store" });
    } catch (e) {
      throw new AdaError("coordinator_unreachable", 0, `adad could not be reached: ${e instanceof Error ? e.message : String(e)}`);
    }
    if (!res.ok) {
      if (res.status === 401 && !openRoutes.test(path)) sessionEnded();
      throw await refusal(res);
    }
    return res;
  };
  const call = async <T>(method: string, path: string, request: Request = {}): Promise<T> => {
    const res = await send(method, path, request);
    try {
      return (await res.json()) as T;
    } catch {
      throw new AdaError("bad_response", res.status, "adad's answer was not the JSON this call needs");
    }
  };
  const none = async (method: string, path: string, request: Request = {}): Promise<void> => {
    await send(method, path, request);
  };

  const readText = async (path: string) => {
    const res = await send("GET", path);
    const truncated = res.headers.get("X-Ada-Truncated") === "true";
    const reader = res.body?.getReader();
    if (!reader) {
      const text = await res.text();
      return { text, truncated, partial: false };
    }
    const chunks: Uint8Array[] = [];
    let size = 0;
    let partial = false;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      const room = checkLogLimit - size;
      if (value.length > room) {
        chunks.push(value.subarray(0, room));
        partial = true;
        await reader.cancel();
        break;
      }
      chunks.push(value);
      size += value.length;
    }
    const joined = new Uint8Array(chunks.reduce((n, c) => n + c.length, 0));
    let at = 0;
    for (const c of chunks) {
      joined.set(c, at);
      at += c.length;
    }
    return { text: new TextDecoder().decode(joined), truncated, partial };
  };

  // One attempt's events. Recorded ones are replayed through the paged JSON form, which keeps
  // every kind, an old database's included; then an EventSource after the last of them carries the
  // live tail, which the executor writes in the kinds listed above. The browser reconnects a
  // dropped stream itself with Last-Event-ID. A refused open closes the EventSource: the session is
  // checked, and while it lives a new replay and EventSource follow after a backoff. cursor_ahead
  // in either form discards the cursor and the view and starts again from 0 with a NEW
  // EventSource, which sends no Last-Event-ID.
  const subscribeEvents = (attemptId: string, onMessage: (message: StreamMessage) => void): (() => void) => {
    let closed = false;
    let source: EventSource | undefined;
    let last = 0;
    let backoff = initial;
    let cancelTimer: (() => void) | undefined;
    if (!Source) {
      onMessage({ type: "status", status: "unreachable", code: "coordinator_unreachable" });
      return () => {};
    }
    const deliver = (event: RecordedEvent) => {
      backoff = initial;
      last = event.seq;
      onMessage({ type: "event", event });
    };
    const reset = (lastSequence: number) => {
      last = 0;
      onMessage({ type: "reset", last_sequence: lastSequence });
    };
    const end = () => {
      closed = true;
      cancelTimer?.();
      source?.close();
      onMessage({ type: "status", status: "unreachable", code: "unauthenticated" });
    };
    const retry = () => {
      if (closed) return;
      onMessage({ type: "status", status: "reconnecting" });
      cancelTimer = schedule(() => void open(), backoff);
      backoff = Math.min(backoff * 2, max);
    };
    // replay reads the recorded events after last, page by page; false once the subscription ended.
    const replay = async (): Promise<boolean> => {
      for (;;) {
        if (closed) return false;
        let page: RecordedEvent[];
        try {
          page = await call<RecordedEvent[]>("GET", `/v1/attempts/${id(attemptId)}/events?after=${last}`);
        } catch (e) {
          if (closed) return false;
          if (lostSession(e)) {
            end();
            return false;
          }
          if (e instanceof AdaError && e.code === "cursor_ahead" && last > 0) {
            reset(typeof e.details?.last_sequence === "number" ? e.details.last_sequence : 0);
            continue;
          }
          throw e;
        }
        if (closed) return false;
        if (!Array.isArray(page) || page.length === 0) return true;
        for (const event of page) {
          if (closed) return false;
          deliver({ seq: event.seq, kind: event.kind, payload: event.payload });
        }
      }
    };
    const open = async () => {
      if (closed) return;
      cancelTimer = undefined;
      try {
        if (!(await replay())) return;
      } catch (e) {
        // A refusal of the attempt itself (not found, forbidden) is final; the rest may pass.
        if (e instanceof AdaError && e.status >= 400 && e.status < 500 && e.status !== 408 && e.status !== 429) {
          closed = true;
          onMessage({ type: "status", status: "unreachable", code: e.code });
          return;
        }
        retry();
        return;
      }
      if (closed) return;
      const es = new Source(`/v1/attempts/${id(attemptId)}/events?after=${last}`);
      source = es;
      const live = () => !closed && source === es;
      es.onopen = () => {
        if (live()) onMessage({ type: "status", status: "connected" });
      };
      es.onerror = (e: Event) => {
        // An event named "error" from the engine reaches onerror as well; its listener takes it.
        if (typeof (e as MessageEvent).data === "string" || !live()) return;
        if (es.readyState !== 2) {
          onMessage({ type: "status", status: "reconnecting" });
          return;
        }
        // A refused open says nothing about why: a lost session ends the subscription.
        onMessage({ type: "status", status: "reconnecting" });
        call("GET", "/v1/account").then(
          () => live() && retry(),
          (err: unknown) => {
            if (!live()) return;
            if (lostSession(err)) end();
            else retry();
          },
        );
      };
      for (const kind of eventKinds) {
        es.addEventListener(kind, (e: Event) => {
          const m = e as MessageEvent;
          if (!live() || typeof m.data !== "string" || m.lastEventId === "") return;
          const seq = Number(m.lastEventId);
          if (!Number.isSafeInteger(seq)) return;
          let payload: unknown;
          try {
            payload = JSON.parse(m.data);
          } catch {
            payload = m.data;
          }
          deliver({ seq, kind, payload });
        });
      }
      es.addEventListener("cursor_ahead", (e: Event) => {
        if (!live()) return;
        es.close();
        let lastSequence = 0;
        try {
          const parsed: unknown = JSON.parse((e as MessageEvent).data as string);
          const value = (parsed as { last_sequence?: unknown } | null)?.last_sequence;
          if (typeof value === "number") lastSequence = value;
        } catch {
          // A malformed body still resets to 0.
        }
        reset(lastSequence);
        void open();
      });
    };
    void open();
    return () => {
      if (closed) return;
      closed = true;
      cancelTimer?.();
      source?.close();
    };
  };

  return {
    channel: "network",
    experience: localOnly,
    createTask: (input) => call("POST", "/v1/tasks", { body: input, idempotent: true }),
    listTasks: () => call("GET", "/v1/tasks"),
    getTask: (taskId) => call("GET", `/v1/tasks/${id(taskId)}`),
    ready: (taskId, revision) => call("POST", `/v1/tasks/${id(taskId)}/ready`, { revision, idempotent: true }),
    start: (taskId, revision, policy) => call("POST", `/v1/tasks/${id(taskId)}/start`, { body: policy, revision, idempotent: true }),
    cancel: (taskId, revision) => call("POST", `/v1/tasks/${id(taskId)}/cancel`, { revision, idempotent: true }),
    subscribeEvents,
    stopCoordinator: localOnly,
    finishQuit: localOnly,
    onQuitRequested: () => () => {},
    capabilities: () => call("GET", "/v1/capabilities"),
    chooseFolder: localOnly,
    inspectFolder: localOnly,
    importFolder: localOnly,
    snapshot: localOnly,
    changes: (taskId) => call("GET", `/v1/tasks/${id(taskId)}/changes`),
    files: (taskId, path) => call("GET", `/v1/tasks/${id(taskId)}/files?path=${encodeURIComponent(path)}`),
    checks: (taskId) => call("GET", `/v1/tasks/${id(taskId)}/checks`),
    checkLog: (taskId, checkId) => readText(`/v1/tasks/${id(taskId)}/checks/${id(checkId)}/log`),
    findings: (taskId) => call("GET", `/v1/tasks/${id(taskId)}/findings`),
    addFinding: (taskId, body) => call("POST", `/v1/tasks/${id(taskId)}/findings`, { body: { body }, idempotent: true }),
    accept: (taskId, revision, commit, generation, secrets) => call("POST", `/v1/tasks/${id(taskId)}/accept`, { body: { commit, generation, secrets }, revision, idempotent: true }),
    refine: (taskId, revision, request) => call("POST", `/v1/tasks/${id(taskId)}/refine`, { body: request, revision, idempotent: true }),
    listAccounts: localOnly,
    createAccount: localOnly,
    updateAccount: localOnly,
    removeAccount: localOnly,
    issueEnrollmentCode: localOnly,
    recoverAccount: localOnly,
    listSessions: localOnly,
    revokeSession: localOnly,
    listener: localOnly,
    setListener: localOnly,
    installListenerCertificate: localOnly,
    projects: () => call("GET", "/v1/projects"),
    account: () => call("GET", "/v1/account"),
    ownSessions: () => call("GET", "/v1/account/sessions"),
    revokeOwnSession: (sessionId) => none("DELETE", `/v1/account/sessions/${id(sessionId)}`, { emptyObject: true }),
    ownCredentials: () => call("GET", "/v1/account/credentials"),
    removeOwnCredential: (credentialId) => none("DELETE", `/v1/account/credentials/${id(credentialId)}`, { emptyObject: true }),
    logout: () => none("POST", "/v1/session/logout", { emptyObject: true }),
    onSessionEnded: (listener) => {
      sessionListeners.add(listener);
      return () => sessionListeners.delete(listener);
    },
    passkeyLogin: async () => {
      const { ceremony, options: request } = await call<{ ceremony: string; options: unknown }>("POST", "/v1/session/passkey/options", { emptyObject: true });
      const credential = await getPasskey(request);
      const signed = await call<{ account: SignedInAccount }>("POST", "/v1/session/passkey", { body: { ceremony, credential } });
      return signed.account;
    },
    passwordLogin: async (account, password) => (await call<{ account: SignedInAccount }>("POST", "/v1/session/password", { body: { account, password } })).account,
    enrollmentOptions: (code) => call("POST", "/v1/enrollment/options", { body: { code } }),
    enrollPasskey: async (code, start, name) => {
      const credential = await createPasskey(start.options);
      return (await call<{ account: SignedInAccount }>("POST", "/v1/enrollment/passkey", { body: { code, ceremony: start.ceremony, credential, name } })).account;
    },
    enrollPassword: async (code, password) => (await call<{ account: SignedInAccount }>("POST", "/v1/enrollment/password", { body: { code, password } })).account,
  };
}
