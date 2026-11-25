import { afterEach, describe, expect, it } from "vitest";
import { AdaError, type Account, type BridgeApi, type Member, type ListenerStatus, type StreamMessage, type Task } from "../api";
import { createBridgeClient } from "./bridge";

const account: Account = { id: "u1", name: "sam", owner: false, disabled: false, password_allowed: false, credentials: 0, sessions: 0, created: "" };
const listenerOff: ListenerStatus = { enabled: false, address: "", name: "", origin: "", state: "off", reason: "disabled", message: "The network listener is turned off.", certificate: null };
const member: Member = { user: "u1", name: "sam", role: "owner", added: "" };
const task: Task = { id: "t1", project: "p", created_by: "local", objective: "o", criteria: "", state: "draft", revision: 0, created: "", updated: "", attempt: null };

function fakeBridge(overrides: Partial<BridgeApi> = {}): BridgeApi {
  return {
    createTask: async () => ({ ok: true, value: task }),
    listTasks: async () => ({ ok: true, value: [task] }),
    getTask: async () => ({ ok: true, value: task }),
    ready: async () => ({ ok: true, value: task }),
    start: async () => ({ ok: true, value: { ...task, attempt_id: "a1" } }),
    cancel: async () => ({ ok: true, value: task }),
    subscribeEvents: () => () => {},
    stopCounts: async () => ({ ok: true, value: { running: 1, queued: 0, others_running: 1, others_queued: 0 } }),
    stop: async () => ({ ok: false, error: { code: "stop_count_changed", status: 409, message: "the count changed", details: { running: 2, queued: 0, others_running: 2, others_queued: 0 } } }),
    decide: async () => ({ ok: true, value: task }),
    members: async () => ({ ok: true, value: [member] }),
    setMember: async () => ({ ok: true, value: [member, { ...member, user: "u2", name: "kim", role: "member" }] }),
    removeMember: async () => ({ ok: false, error: { code: "last_owner", status: 409, message: "the last owner cannot be removed" } }),
    setDefaultMode: async () => ({ ok: true, value: { id: "p", default_mode: "auto" } }),
    finishQuit: async () => ({ ok: true, value: null }),
    onQuitRequested: () => () => {},
    experience: async () => ({ ok: true, value: { version: 1, enabled: false } }),
    capabilities: async () => ({ ok: true, value: { engine: "e", recorded: "", checks: { status: "unsandboxed", detail: "" }, backends: [] } }),
    importRoots: async () => ({ ok: true, value: [] }),
    browseRoot: async () => ({ ok: true, value: { directories: [], truncated: false } }),
    inspectInRoot: async () => ({ ok: true, value: { blockers: [], findings: [], untracked: [] } }),
    importFromRoot: async () => ({ ok: true, value: { id: "p", name: "repo", role: "owner" } }),
    projectUntracked: async () => ({ ok: true, value: [] }),
    allImportRoots: async () => ({ ok: true, value: [] }),
    addImportRoot: async () => ({ ok: true, value: null }),
    removeImportRoot: async () => ({ ok: true, value: null }),
    grantImportRoot: async () => ({ ok: true, value: null }),
    revokeImportRoot: async () => ({ ok: true, value: null }),
    chooseFolder: async () => ({ ok: true, value: null }),
    inspectFolder: async () => ({ ok: false, error: { code: "folder_expired", status: 0, message: "expired" } }),
    importFolder: async () => ({ ok: false, error: { code: "already_imported", status: 409, message: "already", details: { project: "p9" } } }),
    snapshot: async () => ({ ok: false, error: { code: "malformed_call", status: 0, message: "" } }),
    changes: async () => ({ ok: false, error: { code: "no_changes", status: 409, message: "" } }),
    files: async () => ({ ok: false, error: { code: "no_changes", status: 409, message: "" } }),
    checks: async () => ({ ok: true, value: [] }),
    checkLog: async () => ({ ok: true, value: { text: "", truncated: false, partial: false } }),
    findings: async () => ({ ok: true, value: [] }),
    addFinding: async () => ({ ok: false, error: { code: "bad_request", status: 400, message: "" } }),
    accept: async () => ({ ok: false, error: { code: "stale_review", status: 409, message: "" } }),
    refine: async () => ({ ok: false, error: { code: "provider_configuration_changed", status: 409, message: "", details: { provider_changes: { changes: [], truncated: false } } } }),
    listAccounts: async () => ({ ok: true, value: [account] }),
    createAccount: async () => ({ ok: true, value: { account, code: "c", expires: "e" } }),
    updateAccount: async () => ({ ok: true, value: { ...account, disabled: true } }),
    removeAccount: async () => ({ ok: false, error: { code: "account_in_use", status: 409, message: "in use" } }),
    issueEnrollmentCode: async () => ({ ok: true, value: { code: "c2", expires: "e2" } }),
    recoverAccount: async () => ({ ok: false, error: { code: "local_only", status: 403, message: "local only" } }),
    listSessions: async () => ({ ok: true, value: [{ id: "s1", account: "u1", created: "", last_used: "", address: "192.0.2.4" }] }),
    revokeSession: async () => ({ ok: true, value: null }),
    listener: async () => ({ ok: true, value: listenerOff }),
    setListener: async () => ({ ok: false, error: { code: "name_invalid", status: 400, message: "the name must be a lowercase host name" } }),
    installListenerCertificate: async () => ({ ok: true, value: null }),
    listProjects: async () => ({ ok: true, value: [{ id: "p", name: "repo", role: "owner", created: "", default_mode: "readonly" }] }),
    ...overrides,
  };
}

function install(bridge?: BridgeApi): void {
  (window as { ada?: BridgeApi }).ada = bridge;
}

afterEach(() => install(undefined));

describe("bridge adapter", () => {
  it("is the desktop channel, reads the project list through the bridge, and refuses the network's own methods without a call", async () => {
    const called: string[] = [];
    const bridge = new Proxy(fakeBridge(), { get: (target, name: string) => (called.push(name), target[name as keyof BridgeApi]) });
    install(bridge);
    const client = createBridgeClient();
    expect(client.channel).toBe("desktop");
    expect(await client.projects()).toEqual([{ id: "p", name: "repo", role: "owner", created: "", default_mode: "readonly" }]);
    expect(called).toEqual(["listProjects"]);
    const start = { ceremony: "c", options: {}, account: { id: "u", name: "u", owner: false, password_allowed: false } };
    for (const refused of [client.account(), client.ownSessions(), client.revokeOwnSession("s"), client.ownCredentials(), client.removeOwnCredential("c"), client.logout(), client.passkeyLogin(), client.passwordLogin("u", "p"), client.enrollmentOptions("c"), client.enrollPasskey("c", start, "n"), client.enrollPassword("c", "p")]) {
      await expect(refused).rejects.toMatchObject({ code: "network_only" });
    }
    expect(called).toEqual(["listProjects"]);
  });

  it("returns the bridge's values", async () => {
    install(fakeBridge());
    const client = createBridgeClient();
    expect(await client.listTasks()).toEqual([task]);
    expect((await client.start("t1", 0, { backend: "claude", model: "", effort: "", mode: "readonly" })).attempt_id).toBe("a1");
  });

  it("throws adad's refusal with its code and status", async () => {
    install(fakeBridge({ ready: async () => ({ ok: false, error: { code: "stale_revision", status: 409, message: "stale revision" } }) }));
    const error = await createBridgeClient().ready("t1", 0).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(AdaError);
    expect((error as AdaError).code).toBe("stale_revision");
    expect((error as AdaError).status).toBe(409);
  });

  it("keeps the server's request id on an internal error", async () => {
    install(fakeBridge({ getTask: async () => ({ ok: false, error: { code: "internal", status: 500, message: "internal error", request_id: "req-7" } }) }));
    const error = await createBridgeClient().getTask("t1").catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("internal");
    expect((error as AdaError).requestId).toBe("req-7");
  });

  it("passes stream messages through and hands back the unsubscribe", () => {
    let unsubscribed = false;
    let sink: ((message: StreamMessage) => void) | undefined;
    install(
      fakeBridge({
        subscribeEvents: (attemptId, onMessage) => {
          expect(attemptId).toBe("a1");
          sink = onMessage;
          return () => {
            unsubscribed = true;
          };
        },
      }),
    );
    const received: StreamMessage[] = [];
    const unsubscribe = createBridgeClient().subscribeEvents("a1", (message) => received.push(message));
    sink?.({ type: "event", event: { seq: 1, kind: "accepted", payload: {} } });
    expect(received).toEqual([{ type: "event", event: { seq: 1, kind: "accepted", payload: {} } }]);
    unsubscribe();
    expect(unsubscribed).toBe(true);
  });

  it("keeps the details a refusal carries, such as the project already holding a repository", async () => {
    install(fakeBridge());
    const error = await createBridgeClient().importFolder("h1", "repo").catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("already_imported");
    expect((error as AdaError).details).toEqual({ project: "p9" });
    expect(await createBridgeClient().chooseFolder()).toBeNull();
  });

  it("forwards the account methods with their arguments and hands back their values and refusals", async () => {
    const seen: [string, unknown[]][] = [];
    const record = <T>(name: string, value: T) => async (...args: unknown[]) => {
      seen.push([name, args]);
      return value;
    };
    install(
      fakeBridge({
        updateAccount: record("updateAccount", { ok: true as const, value: { ...account, password_allowed: true } }),
        createAccount: record("createAccount", { ok: true as const, value: { account, code: "c", expires: "e" } }),
      }),
    );
    const client = createBridgeClient();
    expect(await client.listAccounts()).toEqual([account]);
    expect(await client.createAccount({ name: "sam", password_allowed: true })).toEqual({ account, code: "c", expires: "e" });
    expect((await client.updateAccount("u1", { password_allowed: true })).password_allowed).toBe(true);
    expect(seen).toEqual([
      ["createAccount", [{ name: "sam", password_allowed: true }]],
      ["updateAccount", ["u1", { password_allowed: true }]],
    ]);
    expect(await client.issueEnrollmentCode("u1")).toEqual({ code: "c2", expires: "e2" });
    expect((await client.listSessions())[0]?.address).toBe("192.0.2.4");
    expect(await client.revokeSession("s1")).toBeUndefined();
    const inUse = await client.removeAccount("u1").catch((e: unknown) => e);
    expect(inUse).toBeInstanceOf(AdaError);
    expect((inUse as AdaError).code).toBe("account_in_use");
    expect((inUse as AdaError).status).toBe(409);
    expect(((await client.recoverAccount("u1").catch((e: unknown) => e)) as AdaError).code).toBe("local_only");
  });

  it("forwards the listener methods with their arguments and hands back their values and refusals", async () => {
    const seen: [string, unknown[]][] = [];
    const serving: ListenerStatus = { ...listenerOff, enabled: true, state: "serving", reason: "", message: "" };
    install(
      fakeBridge({
        listener: async (...args: unknown[]) => {
          seen.push(["listener", args]);
          return { ok: true as const, value: listenerOff };
        },
        installListenerCertificate: async (...args: unknown[]) => {
          seen.push(["installListenerCertificate", args]);
          return { ok: true as const, value: serving };
        },
      }),
    );
    const client = createBridgeClient();
    expect(await client.listener()).toEqual(listenerOff);
    expect(await client.installListenerCertificate()).toEqual(serving);
    expect(seen).toEqual([
      ["listener", []],
      ["installListenerCertificate", []],
    ]);
    const refused = await client.setListener({ enabled: true, address: "192.0.2.20:8443", name: "Home" }).catch((e: unknown) => e);
    expect(refused).toBeInstanceOf(AdaError);
    expect((refused as AdaError).code).toBe("name_invalid");
    expect((refused as AdaError).status).toBe(400);
    expect((refused as AdaError).message).toBe("the name must be a lowercase host name");
    install(fakeBridge());
    expect(await createBridgeClient().installListenerCertificate()).toBeNull();
  });

  it("forwards the decision, the stop and the membership methods with their arguments", async () => {
    const seen: [string, unknown[]][] = [];
    const base = fakeBridge();
    const bridge = new Proxy(base, {
      get: (target, name: string) => {
        const fn = target[name as keyof BridgeApi] as (...args: unknown[]) => unknown;
        return (...args: unknown[]) => (seen.push([name, args]), fn(...args));
      },
    });
    install(bridge);
    const client = createBridgeClient();
    expect(await client.decide("a1", "allow")).toEqual(task);
    expect(await client.stopCounts()).toEqual({ running: 1, queued: 0, others_running: 1, others_queued: 0 });
    const changed = await client.stop(1).catch((e: unknown) => e);
    expect(changed).toMatchObject({ code: "stop_count_changed", status: 409, details: { others_running: 2 } });
    expect(await client.members("p")).toEqual([member]);
    expect(await client.setMember("p", "u2", "member")).toHaveLength(2);
    expect(await client.removeMember("p", "u1").catch((e: unknown) => e)).toMatchObject({ code: "last_owner", status: 409 });
    expect(await client.setDefaultMode("p", "auto")).toEqual({ id: "p", default_mode: "auto" });
    expect(seen).toEqual([
      ["decide", ["a1", "allow"]],
      ["stopCounts", []],
      ["stop", [1]],
      ["members", ["p"]],
      ["setMember", ["p", "u2", "member"]],
      ["removeMember", ["p", "u1"]],
      ["setDefaultMode", ["p", "auto"]],
    ]);
  });

  it("reports a missing bridge as bridge_unavailable", async () => {
    const error = await createBridgeClient().listTasks().catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("bridge_unavailable");
    expect(((await createBridgeClient().listAccounts().catch((e: unknown) => e)) as AdaError).code).toBe("bridge_unavailable");
  });
});
