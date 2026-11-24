import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import { createBridge, type Bridge, type BridgeEvent } from "./bridge";
import { json, listenFake, readBody, type FakeAdad } from "./test/fakeAdad";

// The owner's account methods: each forwards to its owner-channel /v1 route with its method,
// path and body, every mutation with a fresh idempotency key, and adad's refusals come back
// with their codes.

const pageUrl = "file:///E:/ada/ui/dist/index.html";
const fakes: FakeAdad[] = [];
const bridges: Bridge[] = [];
afterEach(async () => {
  for (const bridge of bridges.splice(0)) bridge.close();
  for (const fake of fakes.splice(0)) await fake.close();
});

function event(): BridgeEvent {
  return {
    senderFrame: { url: pageUrl, parent: null },
    sender: { id: 1, send: () => {}, on: () => undefined, once: () => undefined, isDestroyed: () => false },
  };
}

interface Seen {
  method: string;
  url: string;
  body: string;
  headers: http.IncomingHttpHeaders;
}

const account = { id: "u2", name: "sam", owner: false, disabled: false, password_allowed: false, credentials: 0, sessions: 0, created: "2026-11-23T00:00:00Z" };
const code = { code: "ABCDE12345FGHJK67890", expires: "2026-11-24T00:00:00Z" };

function accountsAdad(seen: Seen, res: http.ServerResponse): void {
  const route = `${seen.method} ${seen.url}`;
  if (route === "GET /v1/accounts") json(res, 200, [account]);
  else if (route === "POST /v1/accounts") json(res, 201, { account, ...code });
  else if (route === "PATCH /v1/accounts/u2") json(res, 200, { ...account, disabled: true });
  else if (route === "DELETE /v1/accounts/u2" || route === "DELETE /v1/sessions/s1") {
    res.writeHead(204);
    res.end();
  } else if (route === "DELETE /v1/accounts/u3") json(res, 409, { error: "account_in_use", message: "the account has history" });
  else if (route === "PATCH /v1/accounts/local") json(res, 409, { error: "owner_account", message: "the owner account cannot be disabled" });
  else if (route === "POST /v1/accounts/u2/enrollment-codes" || route === "POST /v1/accounts/u2/recover") json(res, 201, code);
  else if (route === "GET /v1/sessions") json(res, 200, [{ id: "s1", account: "u2", created: "", last_used: "", address: "192.0.2.4" }]);
  else json(res, 404, { error: "not_found", message: "not found" });
}

async function bridgeTo(answer: (seen: Seen, res: http.ServerResponse) => void): Promise<{ bridge: Bridge; seen: Seen[] }> {
  const seen: Seen[] = [];
  const fake = await listenFake((req, res) => {
    void readBody(req).then((body) => {
      const s = { method: req.method ?? "", url: req.url ?? "", body, headers: req.headers };
      seen.push(s);
      answer(s, res);
    });
  });
  fakes.push(fake);
  const bridge = createBridge({ socketPath: fake.socketPath, pageUrl });
  bridges.push(bridge);
  return { bridge, seen };
}

describe("the account methods through the bridge", () => {
  it("forwards each to its route with its method, path and body, mutations with a fresh idempotency key", async () => {
    const { bridge, seen } = await bridgeTo(accountsAdad);
    const calls: [string, unknown[], unknown][] = [
      ["listAccounts", [], [account]],
      ["createAccount", [{ name: "sam", password_allowed: true }], { account, ...code }],
      ["updateAccount", ["u2", { disabled: true }], { ...account, disabled: true }],
      ["updateAccount", ["u2", { password_allowed: true, disabled: false }], { ...account, disabled: true }],
      ["removeAccount", ["u2"], null],
      ["issueEnrollmentCode", ["u2"], code],
      ["recoverAccount", ["u2"], code],
      ["listSessions", [], [{ id: "s1", account: "u2", created: "", last_used: "", address: "192.0.2.4" }]],
      ["revokeSession", ["s1"], null],
    ];
    for (const [method, args, value] of calls) expect(await bridge.handle(event(), method, args), method).toEqual({ ok: true, value });
    expect(seen.map((s) => [s.method, s.url, s.body])).toEqual([
      ["GET", "/v1/accounts", ""],
      ["POST", "/v1/accounts", JSON.stringify({ name: "sam", password_allowed: true })],
      ["PATCH", "/v1/accounts/u2", JSON.stringify({ disabled: true })],
      ["PATCH", "/v1/accounts/u2", JSON.stringify({ password_allowed: true, disabled: false })],
      ["DELETE", "/v1/accounts/u2", ""],
      ["POST", "/v1/accounts/u2/enrollment-codes", ""],
      ["POST", "/v1/accounts/u2/recover", ""],
      ["GET", "/v1/sessions", ""],
      ["DELETE", "/v1/sessions/s1", ""],
    ]);
    const reads = new Set(["GET"]);
    const keys = seen.filter((s) => !reads.has(s.method)).map((s) => s.headers["idempotency-key"]);
    expect(keys).toHaveLength(7);
    for (const key of keys) expect(key).toMatch(/^[\x21-\x7e]{1,255}$/);
    expect(new Set(keys).size).toBe(keys.length);
    for (const s of seen.filter((s) => reads.has(s.method))) expect(s.headers["idempotency-key"]).toBeUndefined();
  });

  it("hands back account_in_use and owner_account with their codes and statuses", async () => {
    const { bridge } = await bridgeTo(accountsAdad);
    expect(await bridge.handle(event(), "removeAccount", ["u3"])).toEqual({ ok: false, error: { code: "account_in_use", status: 409, message: "the account has history" } });
    expect(await bridge.handle(event(), "updateAccount", ["local", { disabled: true }])).toEqual({ ok: false, error: { code: "owner_account", status: 409, message: "the owner account cannot be disabled" } });
  });

  it("hands back local_only from a coordinator that serves these only to the owner", async () => {
    const { bridge } = await bridgeTo((_seen, res) => json(res, 403, { error: "local_only", message: "owner channel only" }));
    expect(await bridge.handle(event(), "listSessions", [])).toEqual({ ok: false, error: { code: "local_only", status: 403, message: "owner channel only" } });
  });

  it("refuses malformed account calls without reaching adad", async () => {
    const { bridge, seen } = await bridgeTo(accountsAdad);
    const calls: [string, unknown[]][] = [
      ["listAccounts", ["x"]],
      ["listSessions", [{}]],
      ["createAccount", []],
      ["createAccount", [{ name: "", password_allowed: false }]],
      ["createAccount", [{ name: "x".repeat(65), password_allowed: false }]],
      ["createAccount", [{ name: "sam" }]],
      ["createAccount", [{ name: "sam", password_allowed: "yes" }]],
      ["createAccount", [{ name: "sam", password_allowed: false, owner: true }]],
      ["updateAccount", ["u2", {}]],
      ["updateAccount", ["u2", { disabled: "true" }]],
      ["updateAccount", ["u2", { name: "other" }]],
      ["updateAccount", ["../tasks", { disabled: true }]],
      ["removeAccount", ["u2/../../tasks"]],
      ["removeAccount", []],
      ["issueEnrollmentCode", ["u2", "extra"]],
      ["recoverAccount", [42]],
      ["revokeSession", ["s1?all=1"]],
    ];
    for (const [method, args] of calls) {
      expect(await bridge.handle(event(), method, args), `${method} ${JSON.stringify(args)}`).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    expect(seen).toHaveLength(0);
    // A 64-character name, counted in characters, not UTF-16 units, is the most adad takes.
    expect((await bridge.handle(event(), "createAccount", [{ name: "\u{1F600}".repeat(64), password_allowed: false }])).ok).toBe(true);
  });
});
