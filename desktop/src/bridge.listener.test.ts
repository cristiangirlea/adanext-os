import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import { createBridge, type Bridge, type BridgeEvent, type BridgeOptions } from "./bridge";
import { json, listenFake, readBody, type FakeAdad } from "./test/fakeAdad";

// The owner's network listener methods: the status and the setting forward to their
// owner-channel routes, and the certificate's paths come only from main's dialogs, go to adad
// and never back to the renderer.

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

const dir = process.platform === "win32" ? "C:\\certs" : "/certs";
const certificate = process.platform === "win32" ? `${dir}\\home.pem` : `${dir}/home.pem`;
const key = process.platform === "win32" ? `${dir}\\home.key` : `${dir}/home.key`;

const off = { enabled: false, address: "", name: "", origin: "", state: "off", reason: "disabled", message: "The network listener is turned off.", certificate: null };
const serving = {
  enabled: true,
  address: "192.0.2.20:8443",
  name: "home.adanext.si",
  origin: "https://home.adanext.si:8443",
  state: "serving",
  reason: "",
  message: "",
  certificate: { names: ["home.adanext.si"], not_before: "2026-11-01T00:00:00Z", not_after: "2027-02-01T00:00:00Z", sha256: "ab".repeat(32) },
};

function listenerAdad(seen: Seen, res: http.ServerResponse): void {
  const route = `${seen.method} ${seen.url}`;
  if (route === "GET /v1/listener") json(res, 200, off);
  else if (route === "PUT /v1/listener") {
    const body = JSON.parse(seen.body) as { address: string };
    if (body.address === "0.0.0.0:8443") json(res, 400, { error: "address_invalid", message: "the address must be one of this computer's addresses, not 0.0.0.0" });
    else json(res, 200, { ...serving, ...body });
  } else if (route === "PUT /v1/listener/certificate") {
    const body = JSON.parse(seen.body) as { key: string };
    if (body.key.endsWith("old.key")) json(res, 400, { error: "certificate_expired", message: "the certificate expired on 2026-01-01" });
    else json(res, 200, serving);
  } else json(res, 404, { error: "not_found", message: "not found" });
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

describe("the listener methods through the bridge", () => {
  it("reads the status and saves the setting with PUT, without an idempotency key", async () => {
    const { bridge, seen } = await bridgeTo(listenerAdad);
    expect(await bridge.handle(event(), "listener", [])).toEqual({ ok: true, value: off });
    const setting = { enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" };
    expect(await bridge.handle(event(), "setListener", [setting])).toEqual({ ok: true, value: { ...serving, ...setting } });
    expect(seen.map((s) => [s.method, s.url, s.body])).toEqual([
      ["GET", "/v1/listener", ""],
      ["PUT", "/v1/listener", JSON.stringify(setting)],
    ]);
    for (const s of seen) expect(s.headers["idempotency-key"]).toBeUndefined();
    expect(seen[1]!.headers["content-type"]).toBe("application/json");
  });

  it("hands back a refused setting with its code, status and message", async () => {
    const { bridge } = await bridgeTo(listenerAdad);
    expect(await bridge.handle(event(), "setListener", [{ enabled: true, address: "0.0.0.0:8443", name: "home.adanext.si" }])).toEqual({
      ok: false,
      error: { code: "address_invalid", status: 400, message: "the address must be one of this computer's addresses, not 0.0.0.0" },
    });
  });

  it("takes the certificate's paths from main's dialogs, sends them to adad and hands the renderer only the status", async () => {
    const asked: number[] = [];
    const { bridge, seen } = await bridgeTo(listenerAdad, {
      chooseCertificate: async (e) => {
        asked.push(e.sender.id);
        return { certificate, key };
      },
    });
    const result = await bridge.handle(event(3), "installListenerCertificate", []);
    expect(result).toEqual({ ok: true, value: serving });
    expect(JSON.stringify(result)).not.toContain("certs");
    expect(asked).toEqual([3]);
    expect(seen.map((s) => [s.method, s.url, s.body])).toEqual([["PUT", "/v1/listener/certificate", JSON.stringify({ certificate, key })]]);
    expect(seen[0]!.headers["idempotency-key"]).toBeUndefined();
  });

  it("answers a cancelled dialog with null and asks adad nothing", async () => {
    const { bridge, seen } = await bridgeTo(listenerAdad, { chooseCertificate: async () => null });
    expect(await bridge.handle(event(), "installListenerCertificate", [])).toEqual({ ok: true, value: null });
    expect(seen).toHaveLength(0);
  });

  it("hands back a refused certificate with its code and message", async () => {
    const old = process.platform === "win32" ? `${dir}\\old.key` : `${dir}/old.key`;
    const { bridge } = await bridgeTo(listenerAdad, { chooseCertificate: async () => ({ certificate, key: old }) });
    expect(await bridge.handle(event(), "installListenerCertificate", [])).toEqual({ ok: false, error: { code: "certificate_expired", status: 400, message: "the certificate expired on 2026-01-01" } });
  });

  it("refuses a relative or remote path from the dialogs, and a shell without them", async () => {
    let answer: unknown = { certificate: "home.pem", key };
    const { bridge, seen } = await bridgeTo(listenerAdad, { chooseCertificate: async () => answer as { certificate: string; key: string } });
    expect(await bridge.handle(event(), "installListenerCertificate", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    answer = { certificate, key: "" };
    expect(await bridge.handle(event(), "installListenerCertificate", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    if (process.platform === "win32") {
      answer = { certificate: "\\\\server\\share\\home.pem", key };
      expect(await bridge.handle(event(), "installListenerCertificate", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    expect(seen).toHaveLength(0);
    const { bridge: bare, seen: none } = await bridgeTo(listenerAdad);
    expect(await bare.handle(event(), "installListenerCertificate", [])).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    expect(none).toHaveLength(0);
  });

  it("refuses malformed listener calls, and any path from the renderer, without reaching adad", async () => {
    let opened = 0;
    const { bridge, seen } = await bridgeTo(listenerAdad, {
      chooseCertificate: async () => {
        opened++;
        return { certificate, key };
      },
    });
    const ok = { enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" };
    const calls: [string, unknown[]][] = [
      ["listener", ["x"]],
      ["setListener", []],
      ["setListener", [ok, ok]],
      ["setListener", ["enabled"]],
      ["setListener", [[ok]]],
      ["setListener", [{ address: ok.address, name: ok.name }]],
      ["setListener", [{ enabled: true, name: ok.name }]],
      ["setListener", [{ enabled: true, address: ok.address }]],
      ["setListener", [{ ...ok, enabled: "true" }]],
      ["setListener", [{ ...ok, address: 8443 }]],
      ["setListener", [{ ...ok, name: null }]],
      ["setListener", [{ ...ok, origin: "https://home.adanext.si" }]],
      ["setListener", [{ ...ok, name: "a".repeat(256) }]],
      ["installListenerCertificate", [certificate]],
      ["installListenerCertificate", [{ certificate, key }]],
    ];
    for (const [method, args] of calls) {
      expect(await bridge.handle(event(), method, args), `${method} ${JSON.stringify(args)}`).toMatchObject({ ok: false, error: { code: "malformed_call" } });
    }
    expect(await bridge.handle(event(), "installListenerCertificate", [{ path: certificate }])).toMatchObject({ ok: false, error: { code: "path_refused" } });
    expect(opened).toBe(0);
    expect(seen).toHaveLength(0);
  });
});
