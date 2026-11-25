import { afterEach, describe, expect, it } from "vitest";
import type { StreamMessage } from "../api";
import { FakeAdad } from "../../test/fakeAdad";
import { makeTask } from "../../test/fakeClient";
import { createHttpsClient } from "./https";
import { creationOptions, fromBase64url, requestOptions, toBase64url } from "./webauthn";

// The browser adapter against the fake adad: same-origin requests as the network listener takes
// them, adad's refusals in the bridge's shape, the owner's channel never fetched, events through
// EventSource with cursor_ahead's fresh start, and the passkey ceremonies with
// navigator.credentials stubbed.

async function settled(): Promise<void> {
  for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

function client(adad: FakeAdad, schedule?: (fn: () => void, ms: number) => () => void) {
  return createHttpsClient({ fetch: adad.fetch, EventSource: adad.EventSource, schedule });
}

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

function stubCredentials(credentials: Partial<CredentialsContainer>): void {
  Object.defineProperty(navigator, "credentials", { value: credentials, configurable: true });
}
afterEach(() => {
  Object.defineProperty(navigator, "credentials", { value: undefined, configurable: true });
  delete (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential;
});

describe("the HTTPS adapter's requests", () => {
  it("is the network channel and sends a change as JSON from this origin with its revision and an idempotency key", async () => {
    const adad = new FakeAdad([makeTask({ id: "t1", revision: 2 })]);
    const https = client(adad);
    expect(https.channel).toBe("network");
    await https.ready("t1", 2);
    await https.createTask({ project: "p1", objective: "o" }).catch(() => {});
    const [ready, create] = adad.requests;
    expect(ready).toMatchObject({ method: "POST", url: "/v1/tasks/t1/ready", body: "" });
    expect(ready!.headers["content-type"]).toBe("application/json");
    expect(ready!.headers["if-match"]).toBe('"2"');
    expect(ready!.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
    expect(create).toMatchObject({ method: "POST", url: "/v1/tasks", body: JSON.stringify({ project: "p1", objective: "o" }) });
    expect(create!.headers["idempotency-key"]).not.toBe(ready!.headers["idempotency-key"]);
  });

  it("reads with GET and no body, same-origin with the session cookie", async () => {
    const seen: RequestInit[] = [];
    const adad = new FakeAdad();
    const https = createHttpsClient({ fetch: (input, init) => (seen.push(init ?? {}), adad.fetch(input, init)) });
    await https.listTasks();
    await https.projects();
    expect(adad.requests.map((r) => `${r.method} ${r.url}`)).toEqual(["GET /v1/tasks", "GET /v1/projects"]);
    for (const init of seen) {
      expect(init).toMatchObject({ method: "GET", credentials: "same-origin", body: undefined });
      expect((init.headers as Record<string, string>)["Content-Type"]).toBeUndefined();
    }
  });

  it("hands back adad's refusal with its code, status, message, request id and details, as the bridge does", async () => {
    const adad = new FakeAdad([makeTask({ id: "t1" })]);
    adad.refusals["POST /v1/tasks/t1/ready"] = { status: 403, body: { error: "not_on_network", message: "not available on the network yet", request_id: "r9", blockers: [{ code: "b", paths: [], count: 0 }] } };
    await expect(client(adad).ready("t1", 0)).rejects.toMatchObject({ name: "AdaError", code: "not_on_network", status: 403, message: "not available on the network yet", requestId: "r9", details: { blockers: [{ code: "b", paths: [], count: 0 }] } });
  });

  it("says the coordinator is unreachable when fetch fails, and an unreadable answer is bad_response", async () => {
    const down = createHttpsClient({ fetch: async () => Promise.reject(new TypeError("Failed to fetch")) });
    await expect(down.listTasks()).rejects.toMatchObject({ code: "coordinator_unreachable", status: 0 });
    const garbled = createHttpsClient({ fetch: async () => new Response("<html>", { status: 200 }) });
    await expect(garbled.listTasks()).rejects.toMatchObject({ code: "bad_response" });
    const proxy = createHttpsClient({ fetch: async () => new Response("<html>", { status: 502 }) });
    await expect(proxy.listTasks()).rejects.toMatchObject({ code: "http_502", status: 502 });
  });

  it("answers the owner's channel and the desktop's dialogs with local_only and never fetches them, the experience card's included", async () => {
    let fetched = 0;
    const https = createHttpsClient({ fetch: async () => (fetched++, new Response("{}")) });
    const refused = [
      https.experience(),
      https.finishQuit(),
      https.chooseFolder(),
      https.inspectFolder("h"),
      https.importFolder("h", "n"),
      https.snapshot("h", { untracked: [], include_sensitive: [] }),
      https.listAccounts(),
      https.createAccount({ name: "n", password_allowed: false }),
      https.updateAccount("u", { disabled: true }),
      https.removeAccount("u"),
      https.issueEnrollmentCode("u"),
      https.recoverAccount("u"),
      https.listSessions(),
      https.revokeSession("s"),
      https.listener(),
      https.setListener({ enabled: true, address: "a", name: "n" }),
      https.installListenerCertificate(),
      https.members("p1"),
      https.setMember("p1", "u2", "member"),
      https.removeMember("p1", "u2"),
      https.setDefaultMode("p1", "auto"),
    ];
    for (const r of refused) await expect(r).rejects.toMatchObject({ code: "local_only" });
    expect(fetched).toBe(0);
    expect(typeof https.onQuitRequested(() => {})).toBe("function");
  });

  it("tells the app its session ended on a 401 from a signed-in request, never on sign-in's own refusals", async () => {
    const adad = new FakeAdad();
    adad.requireSession = true;
    const https = client(adad);
    let ended = 0;
    const stop = https.onSessionEnded(() => ended++);
    await expect(https.passwordLogin("nobody", "wrong password")).rejects.toMatchObject({ status: 401 });
    expect(ended).toBe(0);
    await expect(https.listTasks()).rejects.toMatchObject({ code: "unauthenticated" });
    expect(ended).toBe(1);
    stop();
    await expect(https.projects()).rejects.toMatchObject({ code: "unauthenticated" });
    expect(ended).toBe(1);
  });

  it("reads a check log as text with adad's truncation mark", async () => {
    const https = createHttpsClient({ fetch: async () => new Response("line 1\n", { status: 200, headers: { "X-Ada-Truncated": "true" } }) });
    expect(await https.checkLog("t1", "c1")).toEqual({ text: "line 1\n", truncated: true, partial: false });
  });

  it("sends sign-out and the own account's removals as JSON, and reads the account", async () => {
    const adad = new FakeAdad();
    adad.signedIn = { id: "u1", name: "sam", owner: false, password_allowed: false };
    adad.ownSessionList = [{ id: "s2", created: "", last_used: "", address: "192.0.2.9", current: false }];
    const https = client(adad);
    expect(await https.account()).toEqual(adad.signedIn);
    await https.revokeOwnSession("s2");
    await https.logout();
    await expect(https.account()).rejects.toMatchObject({ code: "unauthenticated", status: 401 });
    const changes = adad.requests.filter((r) => r.method !== "GET");
    expect(changes.map((r) => `${r.method} ${r.url} ${r.body}`)).toEqual(["DELETE /v1/account/sessions/s2 {}", "POST /v1/session/logout {}"]);
    for (const r of changes) expect(r.headers["content-type"]).toBe("application/json");
  });
});

describe("the HTTPS adapter's event stream", () => {
  it("opens an EventSource from 0, delivers each kind with its sequence, an engine event named error included, and closes on unsubscribe", async () => {
    const adad = new FakeAdad();
    const got: StreamMessage[] = [];
    const unsubscribe = client(adad).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    expect(adad.sources.map((s) => s.url)).toEqual(["/v1/attempts/a1/events?after=0"]);
    adad.emit("a1", 1, "text", "hello");
    adad.emit("a1", 2, "error", { v: 1, type: "event" });
    adad.emit("a1", 3, "result", { ok: true });
    expect(got).toEqual([
      { type: "status", status: "connected" },
      { type: "event", event: { seq: 1, kind: "text", payload: "hello" } },
      { type: "event", event: { seq: 2, kind: "error", payload: { v: 1, type: "event" } } },
      { type: "event", event: { seq: 3, kind: "result", payload: { ok: true } } },
    ]);
    unsubscribe();
    expect(adad.sources[0]!.closed).toBe(true);
  });

  it("on cursor_ahead closes at once, discards the cursor and the view, and reopens from 0 with a new EventSource that sends no Last-Event-ID", async () => {
    const adad = new FakeAdad();
    const got: StreamMessage[] = [];
    client(adad).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    adad.emit("a1", 7, "text", "before");
    adad.cursorAhead("a1", 3);
    await settled();
    const [first, second] = adad.sources;
    expect(first!.closed).toBe(true);
    expect(second).toBeDefined();
    expect(second).not.toBe(first);
    expect(second!.url).toBe("/v1/attempts/a1/events?after=0");
    expect(second!.lastEventIdSent).toBeNull();
    expect(adad.sources).toHaveLength(2);
    expect(got.filter((m) => m.type !== "status")).toEqual([
      { type: "event", event: { seq: 7, kind: "text", payload: "before" } },
      { type: "reset", last_sequence: 3 },
    ]);
    // The closed source's events no longer reach the view.
    first!.emit("text", '"stale"', "8");
    expect(got.some((m) => m.type === "event" && m.event.seq === 8)).toBe(false);
  });

  it("after a refused open checks the session, backs off, then opens a new EventSource after the last delivered sequence", async () => {
    const adad = new FakeAdad();
    adad.signedIn = { id: "u1", name: "sam", owner: false, password_allowed: false };
    const got: StreamMessage[] = [];
    const delays: number[] = [];
    const pending: (() => void)[] = [];
    client(adad, (fn, ms) => (delays.push(ms), pending.push(fn), () => {})).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    adad.history.a1 = [{ seq: 4, kind: "text", payload: "x" }];
    adad.emit("a1", 4, "text", "x");
    adad.sources[0]!.fail();
    expect(got.at(-1)).toEqual({ type: "status", status: "reconnecting" });
    await settled();
    expect(adad.requests.at(-1)!.url).toBe("/v1/account");
    expect(adad.sources).toHaveLength(1);
    pending.shift()!();
    await settled();
    expect(adad.sources[1]!.url).toBe("/v1/attempts/a1/events?after=4");
    adad.sources[1]!.fail();
    await settled();
    pending.shift()!();
    expect(delays).toEqual([500, 1000]);
  });

  it("ends the subscription when the session is gone at a refused open, tells the app, and reconnects no more", async () => {
    const adad = new FakeAdad();
    adad.requireSession = true;
    adad.signedIn = { id: "u1", name: "sam", owner: false, password_allowed: false };
    const got: StreamMessage[] = [];
    const pending: (() => void)[] = [];
    const https = client(adad, (fn) => (pending.push(fn), () => {}));
    let ended = 0;
    https.onSessionEnded(() => ended++);
    https.subscribeEvents("a1", (m) => got.push(m));
    await settled();
    adad.signedIn = null;
    adad.sources[0]!.fail();
    await settled();
    expect(ended).toBe(1);
    expect(pending).toEqual([]);
    expect(adad.sources).toHaveLength(1);
    expect(got.at(-1)).toEqual({ type: "status", status: "unreachable", code: "unauthenticated" });
  });

  it("replays recorded events through the JSON form page by page, an unknown kind as the last one included, then opens the stream after it", async () => {
    const adad = new FakeAdad();
    adad.history.a1 = [
      { seq: 1, kind: "text", payload: "one" },
      { seq: 2, kind: "tool_call", payload: { name: "x" } },
      { seq: 3, kind: "an_old_kind", payload: { from: "M2.3b" } },
    ];
    const got: StreamMessage[] = [];
    client(adad).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    expect(got.filter((m) => m.type === "event")).toEqual([
      { type: "event", event: { seq: 1, kind: "text", payload: "one" } },
      { type: "event", event: { seq: 2, kind: "tool_call", payload: { name: "x" } } },
      { type: "event", event: { seq: 3, kind: "an_old_kind", payload: { from: "M2.3b" } } },
    ]);
    expect(adad.requests.filter((r) => r.url.startsWith("/v1/attempts/a1/events")).map((r) => r.url)).toEqual([
      "/v1/attempts/a1/events?after=0",
      "/v1/attempts/a1/events?after=2",
      "/v1/attempts/a1/events?after=3",
      "/v1/attempts/a1/events?after=3",
    ]);
    expect(adad.sources.map((s) => s.url)).toEqual(["/v1/attempts/a1/events?after=3"]);
  });

  it("after cursor_ahead replays from 0 through the JSON form, then opens the stream after the last replayed event", async () => {
    const adad = new FakeAdad();
    const got: StreamMessage[] = [];
    client(adad).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    adad.history.a1 = [{ seq: 1, kind: "an_old_kind", payload: "kept" }];
    adad.cursorAhead("a1", 1);
    await settled();
    expect(got.filter((m) => m.type !== "status")).toEqual([
      { type: "reset", last_sequence: 1 },
      { type: "event", event: { seq: 1, kind: "an_old_kind", payload: "kept" } },
    ]);
    expect(adad.sources.map((s) => s.url)).toEqual(["/v1/attempts/a1/events?after=0", "/v1/attempts/a1/events?after=1"]);
  });
});

describe("unsubscribing and cursors during a replay", () => {
  it("delivers nothing more and asks for no more pages once unsubscribed from the first event's callback", async () => {
    const adad = new FakeAdad();
    adad.history.a1 = [
      { seq: 1, kind: "text", payload: "one" },
      { seq: 2, kind: "text", payload: "two" },
      { seq: 3, kind: "text", payload: "three" },
    ];
    const got: StreamMessage[] = [];
    let unsubscribe = () => {};
    unsubscribe = client(adad).subscribeEvents("a1", (m) => {
      got.push(m);
      if (m.type === "event") unsubscribe();
    });
    await settled();
    expect(got).toEqual([{ type: "event", event: { seq: 1, kind: "text", payload: "one" } }]);
    expect(adad.requests.map((r) => r.url)).toEqual(["/v1/attempts/a1/events?after=0"]);
    expect(adad.sources).toEqual([]);
  });

  it("resets to 0 when the JSON form answers cursor_ahead during a replay", async () => {
    const adad = new FakeAdad();
    adad.signedIn = { id: "u1", name: "sam", owner: false, password_allowed: false };
    adad.history.a1 = [
      { seq: 1, kind: "text", payload: "one" },
      { seq: 2, kind: "text", payload: "two" },
      { seq: 3, kind: "text", payload: "three" },
    ];
    const got: StreamMessage[] = [];
    const pending: (() => void)[] = [];
    client(adad, (fn) => (pending.push(fn), () => {})).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    // A restored database now holds less than the view has seen.
    adad.history.a1 = [{ seq: 1, kind: "text", payload: "restored" }];
    adad.sources[0]!.fail();
    await settled();
    pending.shift()!();
    await settled();
    expect(got.filter((m) => m.type !== "status").slice(3)).toEqual([
      { type: "reset", last_sequence: 1 },
      { type: "event", event: { seq: 1, kind: "text", payload: "restored" } },
    ]);
    expect(adad.sources.map((s) => s.url)).toEqual(["/v1/attempts/a1/events?after=3", "/v1/attempts/a1/events?after=1"]);
  });
});

describe("a final replay refusal", () => {
  it("ends the subscription with the refusal's code and retries no more", async () => {
    const adad = new FakeAdad();
    adad.refusals["GET /v1/attempts/a1/events"] = { status: 404, body: { error: "not_found", message: "not found" } };
    const got: StreamMessage[] = [];
    const pending: (() => void)[] = [];
    client(adad, (fn) => (pending.push(fn), () => {})).subscribeEvents("a1", (m) => got.push(m));
    await settled();
    expect(got).toEqual([{ type: "status", status: "unreachable", code: "not_found" }]);
    expect(pending).toEqual([]);
    expect(adad.sources).toEqual([]);
    expect(adad.requests).toHaveLength(1);
  });

  it("retries a timed-out or throttled page rather than ending", async () => {
    for (const status of [408, 429]) {
      const adad = new FakeAdad();
      adad.refusals["GET /v1/attempts/a1/events"] = { status, body: { error: "busy", message: "try again" } };
      const got: StreamMessage[] = [];
      const pending: (() => void)[] = [];
      client(adad, (fn) => (pending.push(fn), () => {})).subscribeEvents("a1", (m) => got.push(m));
      await settled();
      expect(pending).toHaveLength(1);
      expect(got.some((m) => m.type === "status" && m.code !== undefined)).toBe(false);
    }
  });
});

describe("the task writes as the desktop bridge sends them", () => {
  const policy = { backend: "claude", model: "", effort: "", mode: "readonly" as const };
  const refine = { ...policy, acknowledge_provider_changes: true };
  const secrets = [{ finding: "s1", kind: "test" as const, reason: "a fixture" }];
  const commit = "a".repeat(40);
  const cases: { name: string; call: (c: ReturnType<typeof createHttpsClient>) => Promise<unknown>; path: string; body: unknown; revision?: string }[] = [
    { name: "start", call: (c) => c.start("t1", 3, policy), path: "/v1/tasks/t1/start", body: policy, revision: '"3"' },
    { name: "cancel", call: (c) => c.cancel("t1", 4), path: "/v1/tasks/t1/cancel", body: "", revision: '"4"' },
    { name: "addFinding", call: (c) => c.addFinding("t1", "a note"), path: "/v1/tasks/t1/findings", body: { body: "a note" } },
    { name: "accept", call: (c) => c.accept("t1", 5, commit, 2, secrets), path: "/v1/tasks/t1/accept", body: { commit, generation: 2, secrets }, revision: '"5"' },
    { name: "refine", call: (c) => c.refine("t1", 6, refine), path: "/v1/tasks/t1/refine", body: refine, revision: '"6"' },
  ];
  for (const c of cases) {
    it(`${c.name}: POST ${c.path} with its body, an idempotency key${c.revision ? " and If-Match" : ""}`, async () => {
      const adad = new FakeAdad();
      await c.call(client(adad)).catch(() => {});
      const [sent] = adad.requests;
      expect(sent!.method).toBe("POST");
      expect(sent!.url).toBe(c.path);
      expect(sent!.body === "" ? "" : JSON.parse(sent!.body)).toEqual(c.body);
      expect(sent!.headers["content-type"]).toBe("application/json");
      expect(sent!.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
      expect(sent!.headers["if-match"]).toBe(c.revision);
    });
  }
});

describe("passkeys in the browser", () => {
  it("converts adad's options from base64url and the credential back to it, where the browser has no JSON helpers", () => {
    expect(toBase64url(fromBase64url("AQID-_8"))).toBe("AQID-_8");
    const request = requestOptions({ challenge: "AQID", rpId: "localhost", allowCredentials: [{ type: "public-key", id: "b2xk" }] });
    expect(new Uint8Array(request.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(new TextDecoder().decode(request.allowCredentials![0]!.id as ArrayBuffer)).toBe("old");
    const creation = creationOptions({ challenge: "BAUG", rp: { id: "localhost", name: "Ada" }, user: { id: "dXNlcg", name: "sam", displayName: "sam" }, pubKeyCredParams: [], excludeCredentials: [{ type: "public-key", id: "b2xk" }] });
    expect(new TextDecoder().decode(creation.user.id as ArrayBuffer)).toBe("user");
    expect(new Uint8Array(creation.challenge as ArrayBuffer)).toEqual(new Uint8Array([4, 5, 6]));
  });

  it("signs in with a passkey: options, the authenticator's assertion as JSON, then the session", async () => {
    const adad = new FakeAdad();
    const sam = { id: "u1", name: "sam", owner: false, password_allowed: false };
    adad.handles[toBase64url(bytes(9, 9))] = sam;
    let asked: CredentialRequestOptions | undefined;
    stubCredentials({
      get: async (options?: CredentialRequestOptions) => {
        asked = options;
        return { id: "cred", rawId: bytes(1, 2), type: "public-key", authenticatorAttachment: "platform", response: { clientDataJSON: bytes(3), authenticatorData: bytes(4), signature: bytes(5), userHandle: bytes(9, 9) }, getClientExtensionResults: () => ({}) } as unknown as Credential;
      },
    });
    expect(await client(adad).passkeyLogin()).toEqual(sam);
    expect(new Uint8Array(asked!.publicKey!.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    const sent = adad.requests.find((r) => r.url === "/v1/session/passkey")!;
    expect(JSON.parse(sent.body)).toEqual({
      ceremony: "login-1",
      credential: { id: "cred", rawId: "AQI", type: "public-key", authenticatorAttachment: "platform", clientExtensionResults: {}, response: { clientDataJSON: "Aw", authenticatorData: "BA", signature: "BQ", userHandle: "CQk" } },
    });
    expect(adad.requests.find((r) => r.url === "/v1/session/passkey/options")!.body).toBe("{}");
  });

  it("uses the browser's own JSON helpers where it has them", async () => {
    const adad = new FakeAdad();
    adad.handles.h = { id: "u1", name: "sam", owner: false, password_allowed: false };
    const parsed: unknown[] = [];
    (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential = { parseRequestOptionsFromJSON: (json: unknown) => (parsed.push(json), { challenge: bytes(0) }) };
    stubCredentials({ get: async () => ({ toJSON: () => ({ id: "x", response: { userHandle: "h" } }) }) as unknown as Credential });
    await client(adad).passkeyLogin();
    expect(parsed).toEqual([{ challenge: "AQID", rpId: "localhost", userVerification: "required", timeout: 60000 }]);
    expect(JSON.parse(adad.requests.at(-1)!.body).credential).toEqual({ id: "x", response: { userHandle: "h" } });
  });

  it("sends nothing when the person cancels the passkey, and says why", async () => {
    const adad = new FakeAdad();
    stubCredentials({ get: async () => Promise.reject(new DOMException("The operation either timed out or was not allowed.", "NotAllowedError")) });
    await expect(client(adad).passkeyLogin()).rejects.toMatchObject({ code: "passkey_unavailable" });
    expect(adad.requests.map((r) => r.url)).toEqual(["/v1/session/passkey/options"]);
  });

  it("enrolls a passkey with the code: the creation options converted, the attestation sent with the code, ceremony and name", async () => {
    const adad = new FakeAdad();
    const sam = { id: "u1", name: "sam", owner: false, password_allowed: true };
    adad.enrollCodes.CODE1 = sam;
    let asked: CredentialCreationOptions | undefined;
    stubCredentials({
      create: async (options?: CredentialCreationOptions) => {
        asked = options;
        return { id: "new", rawId: bytes(7), type: "public-key", response: { clientDataJSON: bytes(1), attestationObject: bytes(2), getTransports: () => ["internal"] }, getClientExtensionResults: () => ({}) } as unknown as Credential;
      },
    });
    const https = client(adad);
    const start = await https.enrollmentOptions("CODE1");
    expect(start.account).toEqual(sam);
    expect(await https.enrollPasskey("CODE1", start, "Laptop")).toEqual(sam);
    expect(new TextDecoder().decode(asked!.publicKey!.user.id as ArrayBuffer)).toBe("user");
    expect(new TextDecoder().decode(asked!.publicKey!.excludeCredentials![0]!.id as ArrayBuffer)).toBe("old");
    const sent = JSON.parse(adad.requests.at(-1)!.body);
    expect(sent).toEqual({ code: "CODE1", ceremony: "enroll-CODE1", name: "Laptop", credential: { id: "new", rawId: "Bw", type: "public-key", clientExtensionResults: {}, response: { clientDataJSON: "AQ", attestationObject: "Ag", transports: ["internal"] } } });
  });
});

describe("the decision and the stop on the network channel", () => {
  it("takes a project's snapshot through its network route with an idempotency key", async () => {
    const adad = new FakeAdad();
    adad.projectList = [{ id: "p1", name: "repo", role: "owner", created: "", default_mode: "readonly" }];
    const taken = await client(adad).snapshotProject("p1", { untracked: [], include_sensitive: [], include_changes: false });
    expect(taken.id).toBe("snap-1");
    const sent = adad.requests.at(-1)!;
    expect(sent).toMatchObject({ method: "POST", url: "/v1/projects/p1/snapshots", body: JSON.stringify({ untracked: [], include_sensitive: [], include_changes: false }) });
    expect(sent.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("sends a decision with an idempotency key and answers with the task", async () => {
    const decision = { actor: "u1", required: true, scope: { backend: "claude", mode: "yolo", workspace: "w1" }, project_default: "readonly", allowed: false, denied: false };
    const adad = new FakeAdad([makeTask({ id: "t1", state: "preparing", attempt: { id: "a1", state: "pending", decision } })]);
    const https = client(adad);
    const task = await https.decide("a1", "deny");
    expect(task.id).toBe("t1");
    const sent = adad.requests.at(-1)!;
    expect(sent).toMatchObject({ method: "POST", url: "/v1/attempts/a1/decision", body: JSON.stringify({ decision: "deny" }) });
    expect(sent.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("reads the stop's counts with GET and stops with the confirmed count of other users' attempts", async () => {
    const adad = new FakeAdad();
    adad.stopCounts = { running: 2, queued: 1, others_running: 1, others_queued: 1 };
    const https = client(adad);
    expect(await https.stopCounts()).toEqual({ running: 2, queued: 1, others_running: 1, others_queued: 1 });
    expect(await https.stop(2)).toMatchObject({ running: 2, queued: 1, reason: "coordinator stopped by the owner" });
    const [read, stop] = adad.requests;
    expect(read).toMatchObject({ method: "GET", url: "/v1/coordinator/stop", body: "" });
    expect(stop).toMatchObject({ method: "POST", url: "/v1/coordinator/stop", body: JSON.stringify({ others: 2 }) });
    expect(stop!.headers["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("hands back stop_count_changed with the new counts and stops nothing", async () => {
    const adad = new FakeAdad();
    adad.stopCounts = { running: 3, queued: 0, others_running: 2, others_queued: 0 };
    const refused = await client(adad).stop(1).catch((e: unknown) => e);
    expect(refused).toMatchObject({ code: "stop_count_changed", status: 409, details: { running: 3, queued: 0, others_running: 2, others_queued: 0 } });
    expect(adad.stopped).toBe(false);
  });
});
