import { mkdtempSync, rmSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

// A fake adad for the desktop's tests: a Node http server on a Windows named pipe or a Unix
// socket, the same transport the shell uses for the real one. Tests hand it a request handler.
let counter = 0;

export interface FakeAdad {
  socketPath: string;
  requests: http.IncomingMessage[];
  close(): Promise<void>;
}

export async function listenFake(handler: (req: http.IncomingMessage, res: http.ServerResponse) => void): Promise<FakeAdad> {
  const requests: http.IncomingMessage[] = [];
  const server = http.createServer((req, res) => {
    requests.push(req);
    handler(req, res);
  });
  let socketPath: string;
  let dir: string | undefined;
  if (process.platform === "win32") {
    socketPath = `\\\\.\\pipe\\ada-test-${process.pid}-${++counter}-${Math.random().toString(16).slice(2)}`;
  } else {
    dir = mkdtempSync(join(tmpdir(), "ada-"));
    socketPath = join(dir, `${++counter}.sock`);
  }
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
  return {
    socketPath,
    requests,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => {
          if (dir) rmSync(dir, { recursive: true, force: true });
          resolve();
        });
      }),
  };
}

export function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve) => {
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (chunk: string) => (body += chunk));
    req.on("end", () => resolve(body));
  });
}

export function json(res: http.ServerResponse, status: number, value: unknown): void {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(value));
}

// Resolves once `check` holds, polling; fails after the timeout.
export async function until(check: () => boolean, what: string, timeoutMs = 5000): Promise<void> {
  const start = Date.now();
  while (!check()) {
    if (Date.now() - start > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}
