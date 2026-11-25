import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import { createBridge, type Bridge, type BridgeEvent } from "./bridge";
import { json, listenFake, readBody, type FakeAdad } from "./test/fakeAdad";

// The permission decision, the owner's confirm-first stop, and a project's membership and default
// mode: each forwards to its /v1 route with its method, path and body, every change with a fresh
// idempotency key, and adad's refusals come back with their codes and details.

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

const members = [{ user: "local", name: "owner", role: "owner", added: "2026-11-01T00:00:00Z" }];
const counts = { running: 2, queued: 1, others_running: 1, others_queued: 1 };
const task = { id: "t1", state: "canceled", revision: 4 };

function decisionsAdad(seen: Seen, res: http.ServerResponse): void {
  const route = `${seen.method} ${seen.url}`;
  if (route === "POST /v1/attempts/a1/decision") json(res, 200, task);
  else if (route === "GET /v1/coordinator/stop") json(res, 200, counts);
  else if (route === "POST /v1/coordinator/stop") {
    const others = (JSON.parse(seen.body) as { others: number }).others;
    if (others !== 2) json(res, 409, { error: "stop_count_changed", message: "the count changed", ...counts });
    else json(res, 202, { running: 2, queued: 1, reason: "coordinator stopped by the owner" });
  } else if (route === "GET /v1/projects/p1/members" || route === "PUT /v1/projects/p1/members/u2" || route === "DELETE /v1/projects/p1/members/u2") json(res, 200, members);
  else if (route === "DELETE /v1/projects/p1/members/local") json(res, 409, { error: "last_owner", message: "the last owner cannot be removed" });
  else if (route === "PUT /v1/projects/p1/default-mode") json(res, 200, { id: "p1", default_mode: "auto" });
  else json(res, 404, { error: "not_found", message: "not found" });
}

async function bridgeTo(answer: (seen: Seen, res: http.ServerResponse) => void, finishQuit?: () => Promise<void>): Promise<{ bridge: Bridge; seen: Seen[] }> {
  const seen: Seen[] = [];
  const fake = await listenFake((req, res) => {
    void readBody(req).then((body) => {
      const s = { method: req.method ?? "", url: req.url ?? "", body, headers: req.headers };
      seen.push(s);
      answer(s, res);
    });
  });
  fakes.push(fake);
  const bridge = createBridge({ socketPath: fake.socketPath, pageUrl, finishQuit });
  bridges.push(bridge);
  return { bridge, seen };
}

describe("the decision, stop and membership methods through the bridge", () => {
  it("forwards each to its route with its method, path and body, changes with a fresh idempotency key", async () => {
    const { bridge, seen } = await bridgeTo(decisionsAdad);
    const calls: [string, unknown[], unknown][] = [
      ["decide", ["a1", "deny"], task],
      ["stopCounts", [], counts],
      ["members", ["p1"], members],
      ["setMember", ["p1", "u2", "member"], members],
      ["removeMember", ["p1", "u2"], members],
      ["setDefaultMode", ["p1", "auto"], { id: "p1", default_mode: "auto" }],
      ["stop", [2], { running: 2, queued: 1, reason: "coordinator stopped by the owner" }],
    ];
    for (const [method, args, value] of calls) expect(await bridge.handle(event(), method, args), method).toEqual({ ok: true, value });
    expect(seen.map((s) => [s.method, s.url, s.body])).toEqual([
      ["POST", "/v1/attempts/a1/decision", JSON.stringify({ decision: "deny" })],
      ["GET", "/v1/coordinator/stop", ""],
      ["GET", "/v1/projects/p1/members", ""],
      ["PUT", "/v1/projects/p1/members/u2", JSON.stringify({ role: "member" })],
      ["DELETE", "/v1/projects/p1/members/u2", ""],
      ["PUT", "/v1/projects/p1/default-mode", JSON.stringify({ mode: "auto" })],
      ["POST", "/v1/coordinator/stop", JSON.stringify({ others: 2 })],
    ]);
    const keys = seen.filter((s) => s.method !== "GET").map((s) => s.headers["idempotency-key"]);
    expect(keys).toHaveLength(5);
    for (const key of keys) expect(key).toMatch(/^[\x21-\x7e]{1,255}$/);
    expect(new Set(keys).size).toBe(keys.length);
    for (const s of seen.filter((s) => s.method === "GET")) expect(s.headers["idempotency-key"]).toBeUndefined();
  });

  it("hands back stop_count_changed with the new counts, finishes nothing, and lets the owner confirm again", async () => {
    let finishes = 0;
    const { bridge, seen } = await bridgeTo(decisionsAdad, async () => {
      finishes++;
    });
    expect(await bridge.handle(event(), "stop", [0])).toEqual({ ok: false, error: { code: "stop_count_changed", status: 409, message: "the count changed", details: counts } });
    expect((await bridge.handle(event(), "finishQuit", [])).ok).toBe(false);
    expect(finishes).toBe(0);
    expect((await bridge.handle(event(), "stop", [2])).ok).toBe(true);
    // An accepted stop is shared by a repeated click.
    expect((await bridge.handle(event(), "stop", [2])).ok).toBe(true);
    expect(seen.filter((s) => s.method === "POST")).toHaveLength(2);
    expect((await bridge.handle(event(), "finishQuit", [])).ok).toBe(true);
    expect(finishes).toBe(1);
  });

  it("hands back last_owner with its code", async () => {
    const { bridge } = await bridgeTo(decisionsAdad);
    expect(await bridge.handle(event(), "removeMember", ["p1", "local"])).toEqual({ ok: false, error: { code: "last_owner", status: 409, message: "the last owner cannot be removed" } });
  });

  it("refuses malformed calls, the old empty stop included, without reaching adad", async () => {
    const { bridge, seen } = await bridgeTo(decisionsAdad);
    const calls: [string, unknown[]][] = [
      ["stopCoordinator", []],
      ["stop", []],
      ["stop", [-1]],
      ["stop", [1.5]],
      ["stop", ["2"]],
      ["stop", [2, "extra"]],
      ["stopCounts", [1]],
      ["decide", ["a1"]],
      ["decide", ["a1", "maybe"]],
      ["decide", ["../tasks", "allow"]],
      ["members", []],
      ["members", ["p1/../accounts"]],
      ["setMember", ["p1", "u2"]],
      ["setMember", ["p1", "u2", "admin"]],
      ["setMember", ["p1", "u2/x", "member"]],
      ["removeMember", ["p1"]],
      ["removeMember", ["p1", "u2?all=1"]],
      ["setDefaultMode", ["p1", "ask"]],
      ["setDefaultMode", ["p1"]],
    ];
    for (const [method, args] of calls) {
      expect(await bridge.handle(event(), method, args), `${method} ${JSON.stringify(args)}`).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    expect(seen).toHaveLength(0);
  });
});
