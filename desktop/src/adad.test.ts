import { afterEach, describe, expect, it } from "vitest";
import { BadResponse, call, Refusal } from "./adad";
import { json, listenFake, type FakeAdad } from "./test/fakeAdad";

const fakes: FakeAdad[] = [];
afterEach(async () => {
  for (const fake of fakes.splice(0)) await fake.close();
});

async function adad(handler: Parameters<typeof listenFake>[0]): Promise<FakeAdad> {
  const fake = await listenFake(handler);
  fakes.push(fake);
  return fake;
}

const task = { id: "t1", project: "p", created_by: "local", objective: "o", criteria: "", state: "draft", revision: 0, created: "", updated: "" };

describe("adad client", () => {
  it("rejects a listing larger than the limit as bad_response, counted in wire bytes", async () => {
    const tasks = Array.from({ length: 40 }, (_, i) => ({ ...task, id: `t${i}`, objective: "ééééééééééé" }));
    const fake = await adad((_req, res) => json(res, 200, tasks));
    const wire = Buffer.byteLength(JSON.stringify(tasks));
    const error = await call(fake.socketPath, { method: "GET", path: "/v1/tasks", limit: wire - 1 }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(BadResponse);
    expect((error as BadResponse).code).toBe("bad_response");
    expect((error as Error).message).toContain("bytes");
    // The same body one byte under the limit in characters but over it in bytes is still refused.
    const characters = JSON.stringify(tasks).length;
    expect(characters).toBeLessThan(wire);
    const over = await call(fake.socketPath, { method: "GET", path: "/v1/tasks", limit: characters }).catch((e: unknown) => e);
    expect(over).toBeInstanceOf(BadResponse);
    expect(await call(fake.socketPath, { method: "GET", path: "/v1/tasks", limit: wire })).toEqual(tasks);
  });

  it("rejects a 2xx whose body is not JSON, or is empty, as bad_response", async () => {
    const fake = await adad((req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(req.url === "/v1/tasks" ? "[{" : "");
    });
    const malformed = await call(fake.socketPath, { method: "GET", path: "/v1/tasks" }).catch((e: unknown) => e);
    expect(malformed).toBeInstanceOf(BadResponse);
    const empty = await call(fake.socketPath, { method: "GET", path: "/v1/tasks/t1" }).catch((e: unknown) => e);
    expect(empty).toBeInstanceOf(BadResponse);
    expect(await call(fake.socketPath, { method: "POST", path: "/v1/coordinator/stop", emptyOk: true })).toBeNull();
  });

  it("rejects a body that ends before it is complete as bad_response", async () => {
    const fake = await adad((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json", "Content-Length": "4096" });
      res.write("[");
      setTimeout(() => res.destroy(), 10);
    });
    const error = await call(fake.socketPath, { method: "GET", path: "/v1/tasks" }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(BadResponse);
    expect(error).not.toBeNull();
  });

  it("keeps the server's request_id on an internal refusal", async () => {
    const fake = await adad((_req, res) => json(res, 500, { error: "internal", message: "internal error", request_id: "req-9" }));
    const error = await call(fake.socketPath, { method: "GET", path: "/v1/tasks" }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(Refusal);
    expect((error as Refusal).code).toBe("internal");
    expect((error as Refusal).requestId).toBe("req-9");
    const plain = await adad((_req, res) => json(res, 404, { error: "not_found", message: "not found" }));
    const refusal = (await call(plain.socketPath, { method: "GET", path: "/v1/tasks/t9" }).catch((e: unknown) => e)) as Refusal;
    expect(refusal.requestId).toBeUndefined();
  });
});
