import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import { createBridge, type Bridge, type BridgeEvent, type BridgeOptions } from "./bridge";
import { json, listenFake, readBody, type FakeAdad } from "./test/fakeAdad";

// The workspace's bridge methods (M4.4b): the import flow through main's folder dialog, where a
// path never comes from the renderer, and the review routes of M4.3.

const pageUrl = "file:///E:/ada/ui/dist/index.html";
const fakes: FakeAdad[] = [];
const bridges: Bridge[] = [];
afterEach(async () => {
  for (const bridge of bridges.splice(0)) bridge.close();
  for (const fake of fakes.splice(0)) await fake.close();
});

function event(id = 1): BridgeEvent {
  return {
    senderFrame: { url: pageUrl, parent: null },
    sender: { id, send: () => {}, on: () => undefined, once: () => undefined, isDestroyed: () => false },
  };
}

interface Seen {
  method: string;
  url: string;
  body: string;
  headers: http.IncomingHttpHeaders;
}

async function bridgeTo(answer: (seen: Seen, res: http.ServerResponse) => void, options: Partial<BridgeOptions> = {}): Promise<{ bridge: Bridge; seen: Seen[] }> {
  const seen: Seen[] = [];
  const fake = await listenFake((req, res) => {
    void readBody(req).then((body) => {
      const s = { method: req.method ?? "", url: req.url ?? "", body, headers: req.headers };
      seen.push(s);
      answer(s, res);
    });
  });
  fakes.push(fake);
  const bridge = createBridge({ socketPath: fake.socketPath, pageUrl, ...options });
  bridges.push(bridge);
  return { bridge, seen };
}

const repo = process.platform === "win32" ? "C:\\work\\repo" : "/work/repo";

function importAdad(seen: Seen, res: http.ServerResponse): void {
  if (seen.url === "/v1/projects/inspect") json(res, 200, { identity: { path: repo }, blockers: [], findings: [] });
  else if (seen.url === "/v1/projects") json(res, 201, { id: "p1", name: "repo", role: "owner" });
  else if (seen.url === "/v1/projects/p1/snapshots") json(res, 201, { id: "s1", project: "p1" });
  else if (seen.url === "/v1/tasks") json(res, 201, { id: "t1", project: "p1" });
  else json(res, 404, { error: "not_found", message: "not found" });
}

describe("the import flow through the bridge", () => {
  it("takes the path from main's dialog only and sends it to inspect and import, never to the renderer's choice", async () => {
    const { bridge, seen } = await bridgeTo(importAdad, { chooseFolder: async () => repo });
    const chosen = await bridge.handle(event(), "chooseFolder", []);
    expect(chosen).toMatchObject({ ok: true, value: { path: repo } });
    const handle = (chosen as { value: { handle: string } }).value.handle;
    expect(handle).toMatch(/^[A-Za-z0-9_-]{1,128}$/);
    expect(seen).toHaveLength(0);

    expect((await bridge.handle(event(), "inspectFolder", [handle])).ok).toBe(true);
    expect(seen[0]).toMatchObject({ method: "POST", url: "/v1/projects/inspect", body: JSON.stringify({ path: repo }) });
    expect((await bridge.handle(event(), "importFolder", [handle, "repo"])).ok).toBe(true);
    expect(seen[1]).toMatchObject({ method: "POST", url: "/v1/projects", body: JSON.stringify({ path: repo, name: "repo" }) });
    expect(seen[1]!.headers["idempotency-key"]).toBeTruthy();

    const selection = { untracked: ["notes.txt", ".env"], include_sensitive: [".env"] };
    expect((await bridge.handle(event(), "snapshot", [handle, selection])).ok).toBe(true);
    expect(seen[2]).toMatchObject({ method: "POST", url: "/v1/projects/p1/snapshots", body: JSON.stringify(selection) });

    const created = await bridge.handle(event(), "createTask", [{ project: "p1", objective: "o", snapshot: "s1", checks: [{ name: "tests", argv: ["go", "test", "./..."] }] }]);
    expect(created.ok).toBe(true);
    expect(seen[3]!.body).toBe(JSON.stringify({ project: "p1", objective: "o", snapshot: "s1", checks: [{ name: "tests", argv: ["go", "test", "./..."] }] }));
    // The handle is released once its task exists.
    expect(await bridge.handle(event(), "inspectFolder", [handle])).toMatchObject({ ok: false, error: { code: "folder_expired" } });
  });

  it("passes the owner's choice to leave the uncommitted changes out of the snapshot", async () => {
    const { bridge, seen } = await bridgeTo(importAdad, { chooseFolder: async () => repo });
    const handle = ((await bridge.handle(event(), "chooseFolder", [])) as { value: { handle: string } }).value.handle;
    await bridge.handle(event(), "importFolder", [handle, "repo"]);
    const selection = { untracked: ["notes.txt"], include_sensitive: [], include_changes: false };
    expect((await bridge.handle(event(), "snapshot", [handle, selection])).ok).toBe(true);
    expect(seen.at(-1)).toMatchObject({ url: "/v1/projects/p1/snapshots", body: JSON.stringify(selection) });
    expect(await bridge.handle(event(), "snapshot", [handle, { ...selection, include_changes: "no" }])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
  });

  it("refuses a renderer call that types a path, before reaching adad", async () => {
    const { bridge, seen } = await bridgeTo(importAdad, { chooseFolder: async () => repo });
    const chosen = await bridge.handle(event(), "chooseFolder", []);
    const handle = (chosen as { value: { handle: string } }).value.handle;
    const calls: [string, unknown[]][] = [
      ["inspectFolder", [repo]],
      ["inspectFolder", [{ path: repo }]],
      ["inspectFolder", [handle, { path: repo }]],
      ["importFolder", [{ path: repo, name: "repo" }]],
      ["importFolder", [handle, { path: repo }]],
      ["importFolder", [handle, "repo", repo]],
      ["importFolder", [repo, "repo"]],
      ["snapshot", [handle, { untracked: [], include_sensitive: [], path: repo }]],
      ["chooseFolder", [repo]],
      ["chooseFolder", [{ path: repo }]],
      ["createTask", [{ project: "p1", objective: "o", path: repo }]],
    ];
    for (const [method, args] of calls) {
      const result = await bridge.handle(event(), method, args);
      expect(result.ok, `${method} ${JSON.stringify(args)}`).toBe(false);
      expect((result as { error: { code: string } }).error.code).toMatch(/^(malformed_call|path_refused)$/);
    }
    for (const [method, args] of calls.filter(([, a]) => a.some((x) => typeof x === "object" && x !== null && "path" in x))) {
      expect((await bridge.handle(event(), method, args) as { error: { code: string } }).error.code, method).toBe("path_refused");
    }
    expect(seen).toHaveLength(0);
  });

  it("answers a closed dialog with null and refuses a handle from another window, an unknown one and an expired one", async () => {
    let now = 1000;
    let answer: string | null = null;
    const { bridge, seen } = await bridgeTo(importAdad, { chooseFolder: async () => answer, now: () => now });
    expect(await bridge.handle(event(), "chooseFolder", [])).toEqual({ ok: true, value: null });
    answer = repo;
    const handle = ((await bridge.handle(event(), "chooseFolder", [])) as { value: { handle: string } }).value.handle;
    expect(await bridge.handle(event(2), "inspectFolder", [handle])).toMatchObject({ ok: false, error: { code: "folder_expired" } });
    expect(await bridge.handle(event(), "inspectFolder", ["nope"])).toMatchObject({ ok: false, error: { code: "folder_expired" } });
    now += 16 * 60 * 1000;
    expect(await bridge.handle(event(), "inspectFolder", [handle])).toMatchObject({ ok: false, error: { code: "folder_expired" } });
    expect(seen).toHaveLength(0);
  });

  it("refuses a relative or remote path from the dialog and a snapshot before import", async () => {
    let answer = "relative/repo";
    const { bridge, seen } = await bridgeTo(importAdad, { chooseFolder: async () => answer });
    expect(await bridge.handle(event(), "chooseFolder", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    if (process.platform === "win32") {
      answer = "\\\\server\\share\\repo";
      expect(await bridge.handle(event(), "chooseFolder", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    answer = repo;
    const handle = ((await bridge.handle(event(), "chooseFolder", [])) as { value: { handle: string } }).value.handle;
    expect(await bridge.handle(event(), "snapshot", [handle, { untracked: [], include_sensitive: [] }])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    expect(seen).toHaveLength(0);
  });

  it("keeps the project of an already imported repository on the handle, so the snapshot can follow", async () => {
    const { bridge, seen } = await bridgeTo((s, res) => {
      if (s.url === "/v1/projects") json(res, 409, { error: "already_imported", message: "this repository is already imported", project: "p9", tasks: [{ id: "t9", objective: "earlier", state: "accepted" }] });
      else json(res, 201, { id: "s2", project: "p9" });
    }, { chooseFolder: async () => repo });
    const handle = ((await bridge.handle(event(), "chooseFolder", [])) as { value: { handle: string } }).value.handle;
    expect(await bridge.handle(event(), "importFolder", [handle, ""])).toEqual({
      ok: false,
      error: { code: "already_imported", status: 409, message: "this repository is already imported", details: { project: "p9", tasks: [{ id: "t9", objective: "earlier", state: "accepted" }] } },
    });
    expect((await bridge.handle(event(), "snapshot", [handle, { untracked: [], include_sensitive: [] }])).ok).toBe(true);
    expect(seen[1]!.url).toBe("/v1/projects/p9/snapshots");
  });

  it("hands an import's blockers and a snapshot's refused path back with the refusal", async () => {
    const blockers = [{ code: "escaping_link", paths: ["out"], count: 1 }];
    const { bridge } = await bridgeTo((s, res) => {
      if (s.url === "/v1/projects") json(res, 201, { id: "p1", name: "repo", role: "owner" });
      else if (s.url === "/v1/projects/inspect") json(res, 409, { error: "import_blocked", message: "blocked", blockers });
      else json(res, 422, { error: "sensitive_path", message: "looks like a credential", path: ".env" });
    }, { chooseFolder: async () => repo });
    const handle = ((await bridge.handle(event(), "chooseFolder", [])) as { value: { handle: string } }).value.handle;
    expect(await bridge.handle(event(), "inspectFolder", [handle])).toMatchObject({ ok: false, error: { code: "import_blocked", details: { blockers } } });
    await bridge.handle(event(), "importFolder", [handle, "repo"]);
    expect(await bridge.handle(event(), "snapshot", [handle, { untracked: [".env"], include_sensitive: [] }])).toMatchObject({ ok: false, error: { code: "sensitive_path", details: { path: ".env" } } });
  });
});

describe("the review routes through the bridge", () => {
  it("forwards capabilities, changes, files, checks, findings, accept and refine to their /v1 routes", async () => {
    const { bridge, seen } = await bridgeTo((s, res) => json(res, 200, s.url.startsWith("/v1/tasks/t1/findings") && s.method === "GET" ? [] : {}));
    const commit = "a".repeat(40);
    const policy = { backend: "claude", model: "", effort: "", mode: "readonly", acknowledge_provider_changes: false };
    const calls: [string, unknown[]][] = [
      ["capabilities", []],
      ["changes", ["t1"]],
      ["files", ["t1", ""]],
      ["files", ["t1", "src/a b#?.ts"]],
      ["checks", ["t1"]],
      ["findings", ["t1"]],
      ["addFinding", ["t1", "the test is missing"]],
      ["accept", ["t1", 4, commit, 2]],
      ["refine", ["t1", 4, policy]],
      ["refine", ["t1", 4, { ...policy, mode: "auto", timeout: "5m", max_turns: 3 }]],
    ];
    for (const [method, args] of calls) expect((await bridge.handle(event(), method, args)).ok, method).toBe(true);
    expect(seen.map((s) => `${s.method} ${s.url}`)).toEqual([
      "GET /v1/capabilities",
      "GET /v1/tasks/t1/changes",
      "GET /v1/tasks/t1/files?path=",
      "GET /v1/tasks/t1/files?path=src%2Fa%20b%23%3F.ts",
      "GET /v1/tasks/t1/checks",
      "GET /v1/tasks/t1/findings",
      "POST /v1/tasks/t1/findings",
      "POST /v1/tasks/t1/accept",
      "POST /v1/tasks/t1/refine",
      "POST /v1/tasks/t1/refine",
    ]);
    expect(seen[6]!.body).toBe(JSON.stringify({ body: "the test is missing" }));
    expect(seen[7]!.body).toBe(JSON.stringify({ commit, generation: 2 }));
    expect(seen[7]!.headers["if-match"]).toBe('"4"');
    expect(seen[8]!.body).toBe(JSON.stringify(policy));
    expect(seen[8]!.headers["if-match"]).toBe('"4"');
    // A refine takes the caps as a start does.
    expect(seen[9]!.body).toBe(JSON.stringify({ ...policy, mode: "auto", timeout: "5m", max_turns: 3 }));
  });

  it("refuses malformed review calls without reaching adad", async () => {
    const { bridge, seen } = await bridgeTo((_s, res) => json(res, 200, {}));
    const policy = { backend: "claude", model: "", effort: "", mode: "readonly", acknowledge_provider_changes: false };
    const calls: [string, unknown[]][] = [
      ["files", ["t1", "a\u0000b"]],
      ["files", ["t1", 3]],
      ["files", ["t1", "x".repeat(4097)]],
      ["checkLog", ["t1", "../c"]],
      ["addFinding", ["t1", ""]],
      ["addFinding", ["t1", "x".repeat(16385)]],
      ["accept", ["t1", 4, "not-a-commit", 2]],
      ["accept", ["t1", 4, "a".repeat(40), -1]],
      ["refine", ["t1", 4, { ...policy, mode: "ask" }]],
      ["refine", ["t1", 4, { backend: "claude", model: "", effort: "", mode: "readonly" }]],
      ["refine", ["t1", 4, { ...policy, max_turns: 0 }]],
      ["refine", ["t1", 4, { ...policy, timeout: 300 }]],
      ["createTask", [{ project: "p", objective: "o", checks: [{ argv: "go test" }] }]],
      ["createTask", [{ project: "p", objective: "o", snapshot: "../s" }]],
      ["capabilities", ["x"]],
    ];
    for (const [method, args] of calls) {
      expect(await bridge.handle(event(), method, args), `${method} ${JSON.stringify(args)}`).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    expect(seen).toHaveLength(0);
  });

  it("reads a check log as text, with adad's truncation mark and its own cap", async () => {
    const { bridge, seen } = await bridgeTo((_s, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream", "X-Ada-Truncated": "true" });
      res.end("ok\n<b>not html</b>\n" + "x".repeat(2 << 20));
    });
    const result = await bridge.handle(event(), "checkLog", ["t1", "c1"]);
    expect(seen[0]!.url).toBe("/v1/tasks/t1/checks/c1/log");
    expect(result.ok).toBe(true);
    const log = (result as { value: { text: string; truncated: boolean; partial: boolean } }).value;
    expect(log.text.startsWith("ok\n<b>not html</b>\n")).toBe(true);
    expect(log.text.length).toBe(1 << 20);
    expect(log.truncated).toBe(true);
    expect(log.partial).toBe(true);
  });

  it("hands back a check log's refusal with its request id and details, as every other call does", async () => {
    const { bridge } = await bridgeTo((_s, res) => json(res, 500, { error: "internal", message: "internal error", request_id: "req-9", path: "x" }));
    expect(await bridge.handle(event(), "checkLog", ["t1", "c1"])).toEqual({ ok: false, error: { code: "internal", status: 500, message: "internal error", request_id: "req-9", details: { path: "x" } } });
  });

  it("hands back a refine's provider changes with its refusal", async () => {
    const changes = { changes: [{ path: ".claude/settings.json", change: "added" }], truncated: false };
    const { bridge } = await bridgeTo((_s, res) => json(res, 409, { error: "provider_configuration_changed", message: "changed", provider_changes: changes }));
    const result = await bridge.handle(event(), "refine", ["t1", 4, { backend: "claude", model: "", effort: "", mode: "readonly", acknowledge_provider_changes: false }]);
    expect(result).toMatchObject({ ok: false, error: { code: "provider_configuration_changed", details: { provider_changes: changes } } });
  });
});
