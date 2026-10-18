import { expect, it, vi } from "vitest";
import net from "node:net";
import { createHmac, randomBytes, randomUUID } from "node:crypto";
import { chmod, mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { call, endpoint, findOrStart, ownerOnlyDirectory, probeCoordinator } from "./adad";
import { openEventStream } from "./sse";

async function peer(mode: string) {
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "ada-auth-"));
  await mkdir(path.join(stateDir, "run"));
  const key = randomBytes(32);
  await writeFile(path.join(stateDir, "adad.token"), key, { mode: 0o600 });
  const config = { stateDir, name: `auth-${randomUUID()}`, binary: "never-spawn", args: [] };
  const socketPath = endpoint(config, process.platform, "S-1-5-21-1");
  const sockets = new Set<net.Socket>();
  let payload = "";
  const nonces: string[] = [];
  const server = net.createServer((socket) => {
    sockets.add(socket);
    socket.on("error", () => {});
    let authenticated = false;
    let input = "";
    let timer: ReturnType<typeof setInterval> | undefined;
    socket.on("close", () => { sockets.delete(socket); clearInterval(timer); });
    if (mode === "silent") return;
    if (mode === "trickle") { timer = setInterval(() => socket.write("a"), 20); return; }
    socket.on("data", (chunk) => {
      if (authenticated) {
        payload += chunk.toString();
        if (mode === "http-trickle" && !timer) {
          socket.write("HTTP/1.1 200 OK\r\nX-Trickle: ");
          timer = setInterval(() => socket.write("a"), 20);
        }
        return;
      }
      input += chunk.toString();
      if (!input.includes("\n")) return;
      const match = /^ADANEXT1 ([a-f0-9]{64})\n$/.exec(input);
      if (!match) { socket.destroy(); return; }
      nonces.push(match[1]!);
      authenticated = true;
      if (mode === "malformed") {
        socket.write(Buffer.concat([Buffer.alloc(64, 0xe1), Buffer.from("\n")]));
        return;
      }
      const proof = createHmac("sha256", mode === "wrong" ? randomBytes(32) : key).update("adanext-endpoint-v1").update(Buffer.from(match[1]!, "hex")).digest("hex");
      socket.write(proof + "\n");
    });
  });
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(socketPath, resolve); });
  return { config, socketPath, nonces, payload: () => payload, close: async () => {
    for (const socket of sockets) socket.destroy();
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await rm(stateDir, { recursive: true, force: true });
  } };
}

it("refuses a real wrong-key endpoint before HTTP and never spawns, including SSE", async () => {
  const fake = await peer("wrong");
  const spawn = vi.fn(async () => {});
  try {
    await expect(findOrStart(fake.config, fake.socketPath, { probe: probeCoordinator, spawn, now: Date.now, sleep: async () => {} })).rejects.toThrow("endpoint authentication failed");
    await expect(call(fake.socketPath, { method: "POST", path: "/secret", body: { secret: "must not be sent" } })).rejects.toThrow("endpoint authentication failed");
    let status = "";
    const unavailable = vi.fn(async () => {});
    const stream = openEventStream({ socketPath: fake.socketPath, attemptId: "a", onUnavailable: unavailable, onMessage: (m) => { if (m.type === "status") status = m.code ?? ""; } });
    try { await vi.waitFor(() => expect(status).toBe("endpoint_authentication_failed")); } finally { stream.close(); }
    expect(unavailable).not.toHaveBeenCalled();
    expect(spawn).not.toHaveBeenCalled();
    expect(fake.payload()).toBe("");
    expect(new Set(fake.nonces).size).toBe(3);
  } finally { await fake.close(); }
});

it("refuses non-ASCII proof bytes as endpoint authentication failure", async () => {
  const fake = await peer("malformed");
  try {
    await expect(call(fake.socketPath, { method: "GET", path: "/v1/tasks", timeoutMs: 300 })).rejects.toThrow("endpoint authentication failed");
    expect(fake.payload()).toBe("");
  } finally { await fake.close(); }
});

for (const mode of ["silent", "trickle", "http-silent", "http-trickle"]) {
  it(`bounds a real ${mode} endpoint with an absolute probe deadline and never spawns`, async () => {
    const fake = await peer(mode);
    const spawn = vi.fn(async () => {});
    const start = Date.now();
    try {
      await expect(findOrStart(fake.config, fake.socketPath, { probe: probeCoordinator, spawn, now: Date.now, sleep: async () => {} }, 200)).rejects.toThrow();
      expect(Date.now() - start).toBeLessThan(1000);
      expect(Date.now() - start).toBeGreaterThanOrEqual(100);
      expect(spawn).not.toHaveBeenCalled();
    } finally { await fake.close(); }
  });
}

it("trusts a token only in the user's own 0700 directory", () => {
  const dir = (mode: number, uid = 501, directory = true) => ({ isDirectory: () => directory, uid, mode });
  expect(ownerOnlyDirectory(dir(0o40700), 501)).toBe(true);
  expect(ownerOnlyDirectory(dir(0o40750), 501)).toBe(false);
  expect(ownerOnlyDirectory(dir(0o40755), 501)).toBe(false);
  expect(ownerOnlyDirectory(dir(0o40700, 0), 501)).toBe(false);
  expect(ownerOnlyDirectory(dir(0o120700, 501, false), 501)).toBe(false);
  expect(ownerOnlyDirectory(dir(0o40700), undefined)).toBe(false);
});

it.skipIf(process.platform === "win32")("refuses a token whose directory another account can read, before the handshake", async () => {
  const fake = await peer("good");
  try {
    await chmod(fake.config.stateDir, 0o750);
    await expect(call(fake.socketPath, { method: "GET", path: "/v1/tasks", timeoutMs: 300 })).rejects.toThrow("endpoint authentication failed");
    expect(fake.nonces).toHaveLength(0);
    expect(fake.payload()).toBe("");
  } finally { await fake.close(); }
});
