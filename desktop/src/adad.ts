import { randomUUID } from "node:crypto";
import http from "node:http";

// The main process's HTTP client for adad over the per-user named pipe or Unix socket: Node's
// http with socketPath. It forwards; authorization is adad's (decisions log, 2026-10-14, #5).

export class Refusal extends Error {
  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "Refusal";
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
}

const requestTimeoutMs = 30_000;
const responseLimit = 1 << 20;

export function readJSON(res: http.IncomingMessage, limit: number): Promise<unknown> {
  return new Promise((resolve) => {
    let body = "";
    let over = false;
    res.setEncoding("utf8");
    res.on("data", (chunk: string) => {
      if (over) return;
      body += chunk;
      if (body.length > limit) {
        over = true;
        res.destroy();
      }
    });
    const done = (): void => {
      if (over) {
        resolve(undefined);
        return;
      }
      try {
        resolve(body === "" ? undefined : JSON.parse(body));
      } catch {
        resolve(undefined);
      }
    };
    res.on("end", done);
    res.on("error", () => resolve(undefined));
    res.on("close", done);
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

const unreachableCodes = new Set(["ENOENT", "ECONNREFUSED", "ENOTFOUND", "EACCES", "EPIPE", "ECONNRESET"]);

export function isUnreachable(error: unknown): boolean {
  const code = (error as { code?: unknown } | null)?.code;
  return typeof code === "string" && unreachableCodes.has(code);
}

// call sends one request and resolves with the JSON body of a 2xx answer, or rejects with
// adad's Refusal (its code, status and message) or the transport's error.
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
    const req = http.request({ socketPath, method: request.method, path: request.path, headers }, (res) => {
      void readJSON(res, responseLimit).then((body) => {
        const status = res.statusCode ?? 0;
        if (status >= 200 && status < 300) {
          resolve(body ?? null);
          return;
        }
        reject(new Refusal(errorCode(body, status), status, errorMessage(body, `adad answered ${status}`)));
      });
    });
    req.setTimeout(requestTimeoutMs, () => req.destroy(Object.assign(new Error("adad did not answer in time"), { code: "ETIMEDOUT" })));
    req.on("error", reject);
    req.end(payload);
  });
}
