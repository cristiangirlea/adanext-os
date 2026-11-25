import type { BridgeApi, BridgeResult, OwnCredential, OwnSession, ProjectSummary, SignedInAccount, StreamMessage, Task } from "../client/api";
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
  // Refusals by route, such as "POST /v1/tasks/t1/ready".
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
    if (method === "GET") {
      if (path === "/v1/tasks") return { status: 200, body: this.tasks };
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
      const b = a.body as { error: string; message: string };
      return { ok: false, error: { code: b.error, status: a.status, message: b.message } };
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
