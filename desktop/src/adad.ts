import { randomUUID } from "node:crypto";
import http from "node:http";

// The main process's HTTP client for adad over the per-user named pipe or Unix socket: Node's
// http with socketPath. It forwards; authorization is adad's (decisions log, 2026-10-14, #5).

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
}

const requestTimeoutMs = 30_000;
const responseLimit = 1 << 20;
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
    const req = http.request({ socketPath, method: request.method, path: request.path, headers }, (res) => {
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
    req.setTimeout(requestTimeoutMs, () => req.destroy(Object.assign(new Error("adad did not answer in time"), { code: "ETIMEDOUT" })));
    req.on("error", reject);
    req.end(payload);
  });
}
