import type { Account, BridgeApi, BridgeResult, ErrorDetails, OwnCredential, StopCounts, OwnSession, ProjectSummary, SignedInAccount, StreamMessage, Task } from "../client/api";
import { capabilities } from "./fakeClient";

// One fake adad that both adapters talk to: the desktop adapter through a fake preload bridge,
// the browser adapter through a fetch and EventSource stub. Both read the same state and answer
// through the same routes, so a component test runs unchanged against either shell.

interface Answer {
  status: number;
  body?: unknown;
}

export interface Seen {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: string;
}

export interface FakeSource {
  url: string;
  readyState: number;
  closed: boolean;
  // Whether the stub's own reconnect would send Last-Event-ID: never for a new instance.
  lastEventIdSent: string | null;
  emit(kind: string, data: string, id?: string): void;
  fail(): void;
}

export class FakeAdad {
  tasks: Task[];
  projectList: ProjectSummary[] = [];
  matrix = capabilities();
  // Overrides by route, such as "POST /v1/tasks/t1/ready".
  refusals: Record<string, Answer> = {};
  requests: Seen[] = [];
  sources: FakeSource[] = [];
  // The network channel's accounts: the signed-in one (null: 401), passwords by account name,
  // enrollment codes, and the account each passkey's user handle names.
  signedIn: SignedInAccount | null = null;
  passwords: Record<string, { account: SignedInAccount; password: string }> = {};
  enrollCodes: Record<string, SignedInAccount> = {};
  handles: Record<string, SignedInAccount> = {};
  ownSessionList: OwnSession[] = [];
  // When set, every route but sign-in and enrollment needs a session, as the network listener's
  // do, and an event stream opened without one is refused.
  requireSession = false;
  // Each attempt's recorded events, which the JSON form pages pageSize at a time.
  history: Record<string, { seq: number; kind: string; payload: unknown }[]> = {};
  pageSize = 2;
  credentialList: OwnCredential[] = [];
  // The owner channel's accounts list, which the desktop reads for the owner account's id.
  accountList: Account[] = [];
  // What a stop would end now; stopped once a stop was accepted, finished once the shell quit.
  stopCounts: StopCounts = { running: 0, queued: 0, others_running: 0, others_queued: 0 };
  stopped = false;
  roots: { id: string; label: string; grants: string[] }[] = [];
  snapshots = 0;
  finished = false;
  private bridgeListeners = new Map<string, Set<(message: StreamMessage) => void>>();

  constructor(tasks: Task[] = []) {
    this.tasks = tasks;
  }

  answer(method: string, url: string, headers: Record<string, string>, body = ""): Answer {
    const path = url.split("?")[0]!;
    const refused = this.refusals[`${method} ${path}`];
    if (refused) return refused;
    const network = this.network(method, path, body);
    if (network) return network;
    if (this.requireSession && this.signedIn === null) return { status: 401, body: { error: "unauthenticated", message: "sign in first" } };
    const events = /^\/v1\/attempts\/([^/]+)\/events$/.exec(path);
    if (method === "GET" && events) {
      const all = this.history[decodeURIComponent(events[1]!)] ?? [];
      const after = Number(new URLSearchParams(url.split("?")[1] ?? "").get("after") ?? "0");
      const last = all.at(-1)?.seq ?? 0;
      if (after > last) return { status: 409, body: { error: "cursor_ahead", message: "cursor ahead", last_sequence: last } };
      return { status: 200, body: all.filter((e) => e.seq > after).slice(0, this.pageSize).map((e) => ({ attempt: events[1], time: "", ...e })) };
    }
    if (path === "/v1/coordinator/stop") return this.stop(method, body);
    if (method === "GET" && path.endsWith("/untracked")) return { status: 200, body: [] };
    if (method === "GET" && path === "/v1/import-roots/all") return { status: 200, body: this.roots };
    if (method === "POST" && path === "/v1/import-roots") {
      const root = { id: "r", label: JSON.parse(body).label, grants: [] };
      this.roots.push(root);
      return { status: 201, body: root };
    }
    const [, rootId, account] = /^\/v1\/import-roots\/([^/]+)(?:\/grants\/([^/]+))?$/.exec(path) ?? [];
    const root = this.roots.find(r => r.id === rootId);
    if (root && (method === "DELETE" || method === "PUT")) {
      if (!account) this.roots = this.roots.filter(r => r !== root);
      else root.grants = method === "PUT" ? [...root.grants, account] : root.grants.filter(id => id !== account);
      return { status: 200, body: {} };
    }
    const snapshot = /^\/v1\/projects\/([^/]+)\/snapshots$/.exec(path);
    if (method === "POST" && snapshot) {
      const project = decodeURIComponent(snapshot[1]!);
      if (!this.projectList.some((p) => p.id === project)) return notFound;
      this.snapshots++;
      return { status: 201, body: { id: `snap-${this.snapshots}`, project, base_commit: "b".repeat(40), untracked: [], provider_configuration: null } };
    }
    if (method === "POST" && path === "/v1/tasks") return this.create(body);
    const start = /^\/v1\/tasks\/([^/]+)\/start$/.exec(path);
    if (method === "POST" && start) return this.start(decodeURIComponent(start[1]!), headers, body);
    const decision = /^\/v1\/attempts\/([^/]+)\/decision$/.exec(path);
    if (method === "POST" && decision) return this.decide(decodeURIComponent(decision[1]!), body);
    if (method === "GET") {
      if (path === "/v1/tasks") return { status: 200, body: this.tasks };
      if (path === "/v1/accounts") return { status: 200, body: this.accountList };
      if (path === "/v1/projects") return { status: 200, body: this.projectList };
      if (path === "/v1/capabilities") return { status: 200, body: this.matrix };
      if (path === "/v1/experience") return { status: 200, body: { version: 1, enabled: false } };
      const task = /^\/v1\/tasks\/([^/]+)$/.exec(path);
      if (task) {
        const found = this.tasks.find((t) => t.id === task[1]);
        return found ? { status: 200, body: found } : notFound;
      }
      if (/^\/v1\/tasks\/[^/]+\/(checks|findings)$/.test(path)) return { status: 200, body: [] };
    }
    const transition = /^\/v1\/tasks\/([^/]+)\/(ready|cancel)$/.exec(path);
    if (method === "POST" && transition) {
      const found = this.tasks.find((t) => t.id === transition[1]);
      if (!found) return notFound;
      if (headers["if-match"] !== `"${found.revision}"`) return { status: 409, body: { error: "stale_revision", message: "stale revision" } };
      const updated: Task = { ...found, state: transition[2] === "ready" ? "ready" : "cancel_requested", revision: found.revision + 1 };
      this.tasks = this.tasks.map((t) => (t.id === found.id ? updated : t));
      return { status: 200, body: updated };
    }
    return notFound;
  }

  // A new task, created as the signed-in account (the owner account on the desktop).
  private create(body: string): Answer {
    const input = JSON.parse(body) as { project: string; objective: string; snapshot?: string };
    const task = { ...this.newTask(input), created_by: this.signedIn?.id ?? "local" };
    this.tasks = [...this.tasks, task];
    return { status: 201, body: task };
  }
  private newTask(input: { project: string; objective: string; snapshot?: string }): Task {
    const id = `t-new-${this.tasks.length + 1}`;
    return { id, project: input.project, created_by: "", objective: input.objective, criteria: "", state: "draft", revision: 0, created: "", updated: "", attempt: null, ...(input.snapshot ? { snapshot: input.snapshot } : {}) };
  }
  // A start: a pending attempt, which waits for its actor's decision above the project's default.
  private start(taskId: string, headers: Record<string, string>, body: string): Answer {
    const found = this.tasks.find((t) => t.id === taskId);
    if (!found) return notFound;
    if (headers["if-match"] !== `"${found.revision}"`) return { status: 409, body: { error: "stale_revision", message: "stale revision" } };
    const policy = JSON.parse(body) as { backend: string; mode: string };
    const rank = ["readonly", "auto", "yolo"];
    const fallback = this.projectList.find((p) => p.id === found.project)?.default_mode ?? "readonly";
    const required = rank.indexOf(policy.mode) > rank.indexOf(fallback);
    const actor = this.signedIn?.id ?? "local";
    const attempt = { id: `a-${taskId}`, state: "pending", ...policy, decision: required ? { actor, required, scope: { backend: policy.backend, mode: policy.mode, workspace: "w" }, project_default: fallback, allowed: false, denied: false } : null };
    const updated: Task = { ...found, state: "preparing", revision: found.revision + 1, attempt };
    this.tasks = this.tasks.map((t) => (t.id === taskId ? updated : t));
    return { status: 200, body: { ...updated, attempt_id: attempt.id } };
  }

  // The owner's stop: the counts, then a stop only with the count of others the owner saw.
  private stop(method: string, body: string): Answer {
    const counts = this.stopCounts;
    if (method === "GET") return { status: 200, body: counts };
    const others = (JSON.parse(body === "" ? "{}" : body) as { others?: unknown }).others;
    if (typeof others !== "number") return { status: 400, body: { error: "bad_request", message: "invalid request" } };
    if (others !== counts.others_running + counts.others_queued) return { status: 409, body: { error: "stop_count_changed", message: "the count of other users' work changed", ...counts } };
    this.stopped = true;
    return { status: 202, body: { running: counts.running, queued: counts.queued, reason: "coordinator stopped by the owner" } };
  }

  // A decision on a pending attempt: an allow marks it allowed, a deny cancels it.
  private decide(attemptId: string, body: string): Answer {
    const found = this.tasks.find((t) => t.attempt?.id === attemptId);
    if (!found || !found.attempt) return notFound;
    const current = found.attempt.decision;
    if (found.attempt.state !== "pending" || !current?.required) return { status: 409, body: { error: "invalid_transition", message: "invalid transition" } };
    const allow = (JSON.parse(body) as { decision: string }).decision === "allow";
    const updated: Task = allow
      ? { ...found, revision: found.revision + 1, attempt: { ...found.attempt, decision: { ...current, allowed: true } } }
      : { ...found, state: "canceled", revision: found.revision + 1, attempt: { ...found.attempt, state: "canceled", decision: { ...current, denied: true } } };
    this.tasks = this.tasks.map((t) => (t.id === found.id ? updated : t));
    return { status: 200, body: updated };
  }

  private serve(method: string, url: string, headers: Record<string, string>, body = ""): Answer {
    this.requests.push({ method, url, headers, body });
    return this.answer(method, url, headers, body);
  }

  // The sign-in, enrollment and own-account routes, as internal/api/network.go answers them.
  private network(method: string, path: string, body: string): Answer | undefined {
    const input = (body === "" ? {} : JSON.parse(body)) as Record<string, unknown>;
    const signIn = (account: SignedInAccount | undefined, status = 200): Answer => {
      if (!account) return { status: 401, body: { error: "login_failed", message: "sign-in failed" } };
      this.signedIn = account;
      return { status, body: { account } };
    };
    const route = `${method} ${path}`;
    switch (route) {
      case "POST /v1/session/passkey/options":
        return { status: 200, body: { ceremony: "login-1", options: { challenge: "AQID", rpId: "localhost", userVerification: "required", timeout: 60000 } } };
      case "POST /v1/session/passkey": {
        const handle = ((input.credential as { response?: { userHandle?: string } })?.response?.userHandle ?? "") as string;
        return input.ceremony === "login-1" ? signIn(this.handles[handle]) : { status: 400, body: { error: "ceremony_invalid", message: "the passkey ceremony is unknown or expired" } };
      }
      case "POST /v1/session/password": {
        const entry = this.passwords[input.account as string];
        return signIn(entry && entry.password === input.password && entry.account.password_allowed ? entry.account : undefined);
      }
      case "POST /v1/enrollment/options": {
        const account = this.enrollCodes[input.code as string];
        if (!account) return { status: 400, body: { error: "code_invalid", message: "the enrollment code is unknown, used or expired" } };
        return {
          status: 200,
          body: { ceremony: `enroll-${input.code as string}`, account, options: { challenge: "BAUG", rp: { id: "localhost", name: "Ada" }, user: { id: "dXNlcg", name: account.name, displayName: account.name }, pubKeyCredParams: [{ type: "public-key", alg: -7 }], excludeCredentials: [{ type: "public-key", id: "b2xk" }] } },
        };
      }
      case "POST /v1/enrollment/passkey":
      case "POST /v1/enrollment/password": {
        const account = this.enrollCodes[input.code as string];
        if (!account) return { status: 400, body: { error: "code_invalid", message: "the enrollment code is unknown, used or expired" } };
        if (route.endsWith("password") && !account.password_allowed) return { status: 403, body: { error: "password_not_allowed", message: "the account does not allow a password" } };
        delete this.enrollCodes[input.code as string];
        return signIn(account, 201);
      }
    }
    const own = route === "GET /v1/account" || route === "POST /v1/session/logout" || path.startsWith("/v1/account/");
    if (!own) return undefined;
    if (this.signedIn === null) return { status: 401, body: { error: "unauthenticated", message: "sign in first" } };
    if (route === "GET /v1/account") return { status: 200, body: this.signedIn };
    if (route === "POST /v1/session/logout") {
      this.signedIn = null;
      return { status: 204 };
    }
    if (route === "GET /v1/account/sessions") return { status: 200, body: this.ownSessionList };
    if (route === "GET /v1/account/credentials") return { status: 200, body: this.credentialList };
    const session = /^DELETE \/v1\/account\/sessions\/([^/]+)$/.exec(route);
    if (session) {
      const current = this.ownSessionList.find((s) => s.id === session[1])?.current;
      this.ownSessionList = this.ownSessionList.filter((s) => s.id !== session[1]);
      if (current) this.signedIn = null;
      return { status: 204 };
    }
    const credential = /^DELETE \/v1\/account\/credentials\/([^/]+)$/.exec(route);
    if (credential) {
      this.credentialList = this.credentialList.filter((c) => c.id !== credential[1]);
      this.ownSessionList = [];
      this.signedIn = null;
      return { status: 204 };
    }
    return undefined;
  }

  // A delivered event, to every open subscription of the attempt in either shell.
  emit(attemptId: string, seq: number, kind: string, payload: unknown): void {
    for (const listener of this.bridgeListeners.get(attemptId) ?? []) listener({ type: "event", event: { seq, kind, payload } });
    for (const source of this.open(attemptId)) source.emit(kind, JSON.stringify(payload), String(seq));
  }

  // The stream form's answer to a cursor above the last recorded sequence.
  cursorAhead(attemptId: string, last: number): void {
    for (const listener of this.bridgeListeners.get(attemptId) ?? []) listener({ type: "reset", last_sequence: last });
    for (const source of this.open(attemptId)) source.emit("cursor_ahead", JSON.stringify({ last_sequence: last }));
  }

  private open(attemptId: string): FakeSource[] {
    return this.sources.filter((s) => !s.closed && s.url.startsWith(`/v1/attempts/${encodeURIComponent(attemptId)}/events`));
  }

  // The preload bridge the desktop adapter reads as window.ada.
  bridge(): BridgeApi {
    const result = async (method: string, url: string, headers: Record<string, string> = {}, body?: unknown): Promise<BridgeResult<never>> => {
      const a = this.serve(method, url, headers, body === undefined ? "" : JSON.stringify(body));
      if (a.status >= 200 && a.status < 300) return { ok: true, value: (a.body ?? null) as never };
      const { error, message, ...rest } = a.body as { error: string; message: string } & Record<string, unknown>;
      const refused: BridgeResult<never> = { ok: false, error: { code: error, status: a.status, message } };
      if (Object.keys(rest).length) refused.error.details = rest as ErrorDetails;
      return refused;
    };
    const routes: Partial<Record<keyof BridgeApi, (...args: never[]) => unknown>> = {
      listTasks: () => result("GET", "/v1/tasks"),
      listProjects: () => result("GET", "/v1/projects"),
      getTask: (id: string) => result("GET", `/v1/tasks/${id}`),
      capabilities: () => result("GET", "/v1/capabilities"),
      experience: () => result("GET", "/v1/experience"),
      checks: (id: string) => result("GET", `/v1/tasks/${id}/checks`),
      findings: (id: string) => result("GET", `/v1/tasks/${id}/findings`),
      ready: (id: string, revision: number) => result("POST", `/v1/tasks/${id}/ready`, { "if-match": `"${revision}"`, "idempotency-key": "bridge" }),
      cancel: (id: string, revision: number) => result("POST", `/v1/tasks/${id}/cancel`, { "if-match": `"${revision}"`, "idempotency-key": "bridge" }),
      onQuitRequested: () => () => {},
      allImportRoots: () => result("GET", "/v1/import-roots/all"),
      addImportRoot: (label: string) => result("POST", "/v1/import-roots", {}, { label }),
      removeImportRoot: (id: string) => result("DELETE", `/v1/import-roots/${id}`),
      grantImportRoot: (id: string, user: string) => result("PUT", `/v1/import-roots/${id}/grants/${user}`),
      revokeImportRoot: (id: string, user: string) => result("DELETE", `/v1/import-roots/${id}/grants/${user}`),
      listAccounts: () => result("GET", "/v1/accounts"),
      stopCounts: () => result("GET", "/v1/coordinator/stop"),
      stop: (others: number) => result("POST", "/v1/coordinator/stop", { "idempotency-key": "bridge" }, { others }),
      decide: (attemptId: string, decision: string) => result("POST", `/v1/attempts/${attemptId}/decision`, { "idempotency-key": "bridge" }, { decision }),
      finishQuit: async () => {
        this.finished = true;
        return { ok: true, value: null };
      },
      subscribeEvents: (attemptId: string, onMessage: (message: StreamMessage) => void) => {
        this.requests.push({ method: "GET", url: `/v1/attempts/${attemptId}/events`, headers: {}, body: "" });
        const set = this.bridgeListeners.get(attemptId) ?? new Set();
        set.add(onMessage);
        this.bridgeListeners.set(attemptId, set);
        queueMicrotask(() => onMessage({ type: "status", status: "connected" }));
        return () => set.delete(onMessage);
      },
    };
    return new Proxy({} as BridgeApi, {
      get: (_target, name: string) => routes[name as keyof BridgeApi] ?? (async () => ({ ok: false, error: { code: "not_found", status: 404, message: `the fake has no ${name}` } })),
    });
  }

  // The browser adapter's fetch: same-origin paths only.
  fetch = async (input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> => {
    const url = String(input);
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries((init.headers ?? {}) as Record<string, string>)) headers[k.toLowerCase()] = v;
    const a = this.serve(init.method ?? "GET", url, headers, typeof init.body === "string" ? init.body : "");
    const body = a.status === 204 || a.body === undefined ? null : JSON.stringify(a.body);
    return new Response(body, { status: a.status, headers: { "Content-Type": "application/json" } });
  };

  // The browser adapter's EventSource: opens on the next microtask, records its URL, and is
  // driven by emit and cursorAhead.
  get EventSource(): typeof EventSource {
    const adad = this;
    class Source extends EventTarget implements FakeSource {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readyState = 0;
      closed = false;
      lastEventIdSent: string | null = null;
      onopen: ((e: Event) => void) | null = null;
      onerror: ((e: Event) => void) | null = null;
      onmessage: ((e: MessageEvent) => void) | null = null;
      constructor(readonly url: string) {
        super();
        adad.sources.push(this);
        adad.requests.push({ method: "GET", url, headers: {}, body: "" });
        queueMicrotask(() => {
          if (this.closed) return;
          if (adad.requireSession && adad.signedIn === null) {
            this.fail();
            return;
          }
          this.readyState = 1;
          this.onopen?.(new Event("open"));
        });
      }
      close() {
        this.closed = true;
        this.readyState = 2;
      }
      emit(kind: string, data: string, id = "") {
        const e = new MessageEvent(kind, { data, lastEventId: id });
        this.dispatchEvent(e);
        if (kind === "error") this.onerror?.(e);
      }
      fail() {
        this.readyState = 2;
        this.onerror?.(new Event("error"));
      }
    }
    return Source as unknown as typeof EventSource;
  }
}

const notFound: Answer = { status: 404, body: { error: "not_found", message: "not found" } };
