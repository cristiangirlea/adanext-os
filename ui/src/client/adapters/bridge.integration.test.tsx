/// <reference types="node" />
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { App } from "../../App";
import { createBridgeClient } from "./bridge";
import { createApi } from "../../../../desktop/src/preloadApi";
import { createBridge } from "../../../../desktop/src/bridge";
import { json, listenFake, readBody } from "../../../../desktop/src/test/fakeAdad";
import { makeTask } from "../../test/fakeClient";

// One fake adad behind the real Node HTTP client, bridge, preload API and desktop
// AdaClient adapter. The shared App sees only AdaClient; HTTPS is a later slice.
it("the App subscribes and displays adad's refusals through the complete bridge adapter", async () => {
  const tasks = [makeTask({ id: "old", state: "running", attempt: { id: "a-old", state: "running" } }), makeTask({ id: "draft" })];
  const fake = await listenFake((req, res) => {
    if (req.url === "/v1/attempts/a-old/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write('id: 1\nevent: text\ndata: "from fake adad"\n\n');
    } else if (req.method === "GET") {
      json(res, 200, req.url === "/v1/tasks" ? tasks : tasks.find((t) => req.url === `/v1/tasks/${t.id}`));
    } else {
      void readBody(req).then(() => json(res, 409, { error: "stale_revision", message: "revision changed in adad" }));
    }
  });
  const pageUrl = "file:///app/index.html";
  const bridge = createBridge({ socketPath: fake.socketPath, pageUrl });
  let receive: (id: unknown, message: unknown) => void = () => {};
  const sender = { id: 1, send: (_channel: string, id: unknown, message: unknown) => receive(id, message), on: () => {}, once: () => {}, isDestroyed: () => false };
  window.ada = createApi({ invoke: (method, args) => bridge.handle({ sender, senderFrame: { url: pageUrl, parent: null } }, method, args), onEvent: (listener) => { receive = listener; } }, "integration");
  try {
    render(<App client={createBridgeClient()} />);
    fireEvent.click(await screen.findByText("objective old"));
    await screen.findByText(/from fake adad/);
    expect(fake.requests.some((r) => r.url === "/v1/attempts/a-old/events")).toBe(true);
    fireEvent.click(screen.getByText("objective draft"));
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("stale_revision"));
    expect(screen.getByRole("alert").textContent).toContain("revision changed in adad");
    const request = fake.requests.find((r) => r.method === "POST")!;
    expect(request.headers["if-match"]).toBe('"0"');
    expect(request.headers["idempotency-key"]).toBeTruthy();
  } finally {
    cleanup();
    delete window.ada;
    bridge.close();
    await fake.close();
  }
});
