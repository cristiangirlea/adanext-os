import { afterEach, describe, expect, it } from "vitest";
import { AdaError, type Account, type BridgeApi, type StreamMessage, type Task } from "../api";
import { createBridgeClient } from "./bridge";

const account: Account = { id: "u1", name: "sam", owner: false, disabled: false, password_allowed: false, credentials: 0, sessions: 0, created: "" };
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
    stopCoordinator: async () => ({ ok: true, value: { running: 0, queued: 0, reason: "coordinator stopped by the owner" } }),
    finishQuit: async () => ({ ok: true, value: null }),
    onQuitRequested: () => () => {},
    experience: async () => ({ ok: true, value: { version: 1, enabled: false } }),
    capabilities: async () => ({ ok: true, value: { engine: "e", recorded: "", checks: { status: "unsandboxed", detail: "" }, backends: [] } }),
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
    ...overrides,
  };
}

function install(bridge?: BridgeApi): void {
  (window as { ada?: BridgeApi }).ada = bridge;
}

afterEach(() => install(undefined));

describe("bridge adapter", () => {
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

  it("reports a missing bridge as bridge_unavailable", async () => {
    const error = await createBridgeClient().listTasks().catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("bridge_unavailable");
    expect(((await createBridgeClient().listAccounts().catch((e: unknown) => e)) as AdaError).code).toBe("bridge_unavailable");
  });
});
