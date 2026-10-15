import { createHmac, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import { readFile } from "node:fs/promises";
import http from "node:http";
import { execFile, spawn } from "node:child_process";
import path from "node:path";
import net from "node:net";
import { promisify } from "node:util";

// The main process's HTTP client for adad over the per-user named pipe or Unix socket: Node's
// http with an authenticated connection. Authorization remains adad's (decisions log, 2026-10-14, #5).

export class Refusal extends Error {
  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    // adad's request_id, sent with an internal error so it can be matched to its log.
    readonly requestId?: string,
  ) {
    super(message);
    this.name = "Refusal";
  }
}

// An answer the client could not read: too large, cut short, or not the JSON the call needs.
// It crosses the bridge as bad_response; a 2xx never turns into a value this way.
export class BadResponse extends Error {
  readonly code = "bad_response";
  constructor(message: string) {
    super(message);
    this.name = "BadResponse";
  }
}

export interface Call {
  method: "GET" | "POST";
  path: string;
  body?: unknown;
  // The expected task revision, sent as If-Match in the ETag's quoted form.
  revision?: number;
  // A mutating call carries a fresh Idempotency-Key.
  idempotent?: boolean;
  // The most wire bytes the answer may hold; responseLimit when absent.
  limit?: number;
  // A 2xx may come without a body; the call then resolves null.
  emptyOk?: boolean;
  timeoutMs?: number;
}

const requestTimeoutMs = 30_000;
const responseLimit = 1 << 20;
const endpointStates = new Map<string, string>();

// Main-process configuration only; no renderer-supplied state paths or keys.
export function bindEndpoint(socketPath: string, stateDir: string): string {
  endpointStates.set(socketPath, stateDir);
  return socketPath;
}

export class EndpointAuthenticationError extends Error {
  readonly code = "endpoint_authentication_failed";
  constructor() { super("endpoint authentication failed"); }
}

// No socket is returned to http until the proof has been checked. Returning the
// connecting socket here would let Node flush buffered request headers early.
export function endpointConnection(socketPath: string, timeoutMs = 5000): NonNullable<http.RequestOptions["createConnection"]> {
  return (_options, callback) => {
    const socket = net.createConnection(socketPath);
    let connected = false;
    let settled = false;
    let proof = Buffer.alloc(0);
    const nonce = randomBytes(32);
    const timer = setTimeout(() => finish(new EndpointAuthenticationError()), Math.min(5000, timeoutMs));
    const fail = (error: Error) => finish(connected ? new EndpointAuthenticationError() : error);
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener("error", fail);
      socket.removeListener("data", receive);
      socket.removeListener("end", ended);
      if (error) { socket.destroy(); callback(error, socket); }
      else { callback(null, socket); }
    };
    const ended = () => finish(new EndpointAuthenticationError());
    let key: Buffer;
    const receive = (chunk: Buffer) => {
      proof = Buffer.concat([proof, chunk]);
      if (proof.length > 65) { finish(new EndpointAuthenticationError()); return; }
      if (!proof.includes(10)) return;
      if (!/^[a-f0-9]{64}\n$/.test(proof.toString("utf8"))) { finish(new EndpointAuthenticationError()); return; }
      const expected = createHmac("sha256", key).update("adanext-endpoint-v1").update(nonce).digest();
      if (!timingSafeEqual(Buffer.from(proof.subarray(0, 64).toString(), "hex"), expected)) { finish(new EndpointAuthenticationError()); return; }
      finish();
    };
    socket.on("error", fail);
    socket.once("end", ended);
    socket.once("connect", () => {
      connected = true;
      const dir = endpointStates.get(socketPath);
      if (!dir) { finish(new EndpointAuthenticationError()); return; }
      void readFile(path.join(dir, "adad.token")).then((value) => {
        if (settled) return;
        if (value.length !== 32) { finish(new EndpointAuthenticationError()); return; }
        key = value;
        socket.on("data", receive);
        socket.write(`ADANEXT1 ${nonce.toString("hex")}\n`);
      }, () => finish(new EndpointAuthenticationError()));
    });
    return undefined;
  };
}
// GET /v1/tasks is not paginated: the listing may hold every task of the user, so it is read
// with its own, larger limit.
export const listingLimit = 64 << 20;

// readJSON reads the body up to `limit` wire bytes and parses it; an empty body resolves
// undefined. Overflow, a body cut short and a body that is not JSON reject with BadResponse.
export function readJSON(res: http.IncomingMessage, limit: number): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    let over = false;
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      fn();
    };
    const incomplete = (): BadResponse => new BadResponse(over ? `adad's answer exceeded ${limit} bytes` : "adad's answer ended before its body was complete");
    res.on("data", (chunk: Buffer) => {
      if (over) return;
      size += chunk.length;
      if (size > limit) {
        over = true;
        res.destroy();
        return;
      }
      chunks.push(chunk);
    });
    res.on("end", () =>
      settle(() => {
        if (over || !res.complete) {
          reject(incomplete());
          return;
        }
        const body = Buffer.concat(chunks).toString("utf8");
        if (body === "") {
          resolve(undefined);
          return;
        }
        try {
          resolve(JSON.parse(body));
        } catch {
          reject(new BadResponse("adad's answer is not JSON"));
        }
      }),
    );
    res.on("error", (error: Error) => settle(() => reject(new BadResponse(`adad's answer could not be read: ${error.message}`))));
    res.on("close", () => settle(() => reject(incomplete())));
  });
}

export function errorCode(body: unknown, status: number): string {
  if (body !== null && typeof body === "object" && typeof (body as { error?: unknown }).error === "string") {
    return (body as { error: string }).error;
  }
  return `http_${status}`;
}

function errorMessage(body: unknown, fallback: string): string {
  if (body !== null && typeof body === "object" && typeof (body as { message?: unknown }).message === "string") {
    return (body as { message: string }).message;
  }
  return fallback;
}

function requestId(body: unknown): string | undefined {
  if (body !== null && typeof body === "object" && typeof (body as { request_id?: unknown }).request_id === "string") {
    return (body as { request_id: string }).request_id;
  }
  return undefined;
}

const unreachableCodes = new Set(["ENOENT", "ECONNREFUSED", "ENOTFOUND", "EACCES", "EPIPE", "ECONNRESET"]);

export function isUnreachable(error: unknown): boolean {
  const code = (error as { code?: unknown } | null)?.code;
  return typeof code === "string" && unreachableCodes.has(code);
}

// call sends one request and resolves with the JSON body of a 2xx answer, or rejects with
// adad's Refusal (its code, status, message and request id), BadResponse for an answer it could
// not read, or the transport's error. A refusal whose body cannot be read keeps its status.
export function call(socketPath: string, request: Call): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const headers: http.OutgoingHttpHeaders = { Accept: "application/json" };
    let payload: string | undefined;
    if (request.body !== undefined) {
      payload = JSON.stringify(request.body);
      headers["Content-Type"] = "application/json";
      headers["Content-Length"] = Buffer.byteLength(payload);
    }
    if (request.revision !== undefined) headers["If-Match"] = `"${request.revision}"`;
    if (request.idempotent) headers["Idempotency-Key"] = randomUUID();
    const req = http.request({ createConnection: endpointConnection(socketPath, request.timeoutMs), method: request.method, path: request.path, headers }, (res) => {
      const status = res.statusCode ?? 0;
      const ok = status >= 200 && status < 300;
      readJSON(res, request.limit ?? responseLimit).then(
        (body) => {
          if (!ok) {
            reject(new Refusal(errorCode(body, status), status, errorMessage(body, `adad answered ${status}`), requestId(body)));
            return;
          }
          if (body === undefined && !request.emptyOk) {
            reject(new BadResponse(`adad answered ${status} without a body`));
            return;
          }
          resolve(body ?? null);
        },
        (error: BadResponse) => reject(ok ? error : new Refusal(`http_${status}`, status, `adad answered ${status}: ${error.message}`)),
      );
    });
    const timer = setTimeout(() => req.destroy(Object.assign(new Error("adad did not answer in time"), { code: "ETIMEDOUT" })), request.timeoutMs ?? requestTimeoutMs);
    req.once("close", () => clearTimeout(timer));
    req.on("error", reject);
    req.end(payload);
  });
}

export interface CoordinatorConfig {
  binary: string;
  stateDir: string;
  name: string;
  args: string[];
}

export function coordinatorConfig(env: NodeJS.ProcessEnv, appDir: string, home: string, platform = process.platform): CoordinatorConfig {
  const p = platform === "win32" ? path.win32 : path.posix;
  const base = platform === "win32" ? env.LOCALAPPDATA : (env.XDG_STATE_HOME && p.isAbsolute(env.XDG_STATE_HOME) ? env.XDG_STATE_HOME : p.join(home, ".local", "state"));
  if (!base && !env.ADANEXT_STATE_DIR) throw new Error("LOCALAPPDATA is not set");
  const stateDir = env.ADANEXT_STATE_DIR || p.join(base!, platform === "win32" ? "AdaNext" : "adanext");
  if (!p.isAbsolute(stateDir)) throw new Error("the coordinator state directory must be absolute");
  const name = env.ADANEXT_NAME || "adad";
  const binary = env.ADANEXT_ADAD || p.join(appDir, platform === "win32" ? "adad.exe" : "adad");
  const args = ["-state-dir", stateDir, "-name", name, "-detach"];
  if (env.ADANEXT_ENGINE) args.push("-engine", env.ADANEXT_ENGINE);
  if (env.ADANEXT_ENGINE_SHA256) args.push("-engine-sha256", env.ADANEXT_ENGINE_SHA256);
  return { binary, stateDir, name, args };
}

export function parseSID(output: string): string {
  // whoami /user /fo csv /nh: do not depend on a localized column heading or username.
  const match = /^"(?:[^"]|"")*","(S-1-\d+(?:-\d+)+)"\s*$/.exec(output.trim());
  if (!match?.[1]) throw new Error("whoami did not return one user SID");
  return match[1];
}

export async function currentSID(): Promise<string> {
  const root = process.env.SystemRoot;
  if (!root) throw new Error("SystemRoot is not set");
  const { stdout } = await promisify(execFile)(path.win32.join(root, "System32", "whoami.exe"), ["/user", "/fo", "csv", "/nh"], { windowsHide: true, timeout: 5000, maxBuffer: 4096 });
  return parseSID(stdout);
}

export function endpoint(config: CoordinatorConfig, platform = process.platform, sid?: string): string {
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(config.name)) throw new Error("invalid coordinator endpoint name");
  if (platform === "win32") {
    if (!sid || !/^S-1-\d+(?:-\d+)+$/.test(sid)) throw new Error("a user SID is required");
    return bindEndpoint(`\\\\.\\pipe\\adanext-${config.name}-${sid}`, config.stateDir);
  }
  return bindEndpoint(path.posix.join(config.stateDir, "run", `${config.name}.sock`), config.stateDir);
}

export const detachedOptions = { detached: true, stdio: "ignore", windowsHide: true } as const;

export function spawnCoordinator(binary: string, args: string[], createChild: typeof spawn = spawn): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = createChild(binary, args, detachedOptions);
    child.once("error", reject);
    child.once("spawn", () => { child.unref(); resolve(); });
  });
}

// Only absence permits a launch. Access errors and incompatible replies must stay visible.
function absent(error: unknown): boolean {
  return ["ENOENT", "ECONNREFUSED"].includes((error as NodeJS.ErrnoException).code ?? "");
}
export async function probeCoordinator(socketPath: string, timeoutMs = 500): Promise<boolean> {
  try {
    const value = await call(socketPath, { method: "GET", path: "/v1/tasks", limit: listingLimit, timeoutMs });
    if (!Array.isArray(value)) throw new BadResponse("not a compatible coordinator task list");
    return true;
  } catch (error) {
    if (absent(error)) return false;
    throw error;
  }
}

// For stop, test the listener itself: an HTTP refusal is not evidence of exit.
export function endpointPresent(socketPath: string, timeoutMs = 500): Promise<boolean> {
  return new Promise((resolve, reject) => {
    const socket = net.createConnection(socketPath);
    const timer = setTimeout(() => socket.destroy(Object.assign(new Error("endpoint probe timed out"), { code: "ETIMEDOUT" })), timeoutMs);
    socket.once("connect", () => { socket.destroy(); resolve(true); });
    socket.once("error", (error) => absent(error) ? resolve(false) : reject(error));
    socket.once("close", () => clearTimeout(timer));
  });
}

export interface LifecycleIO {
  probe(socketPath: string, timeoutMs: number): Promise<boolean>;
  spawn(binary: string, args: string[]): Promise<void>;
  now(): number;
  sleep(ms: number): Promise<void>;
}
const lifecycleIO: LifecycleIO = { probe: probeCoordinator, spawn: spawnCoordinator, now: Date.now, sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)) };

export async function findOrStart(config: CoordinatorConfig, socketPath: string, io = lifecycleIO, timeoutMs = 15_000): Promise<void> {
  bindEndpoint(socketPath, config.stateDir);
  const deadline = io.now() + timeoutMs;
  if (await io.probe(socketPath, Math.min(500, timeoutMs))) return;
  await io.spawn(config.binary, config.args);
  while (io.now() < deadline) {
    if (await io.probe(socketPath, Math.min(500, deadline - io.now()))) return;
    await io.sleep(Math.min(100, Math.max(0, deadline - io.now())));
  }
  throw new Error("timed out waiting for adad to start");
}

// The coordinator's worst orderly stop: the executor's stop budget (5 grace periods of 10 s
// plus 5 s), the hub's 10 s drain and the server's 5 s shutdown, with 15 s of margin.
export const QUIT_WAIT_MS = 85_000;

export async function waitForExit(socketPath: string, io: LifecycleIO = { ...lifecycleIO, probe: endpointPresent }, timeoutMs = QUIT_WAIT_MS): Promise<void> {
  const deadline = io.now() + timeoutMs;
  while (io.now() < deadline) {
    if (!await io.probe(socketPath, Math.min(500, deadline - io.now()))) return;
    await io.sleep(Math.min(100, Math.max(0, deadline - io.now())));
  }
  throw new Error("timed out waiting for adad to stop");
}
