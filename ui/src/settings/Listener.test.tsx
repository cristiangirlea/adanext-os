import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { AdaError } from "../client/api";
import { FakeClient, listenerStatus, makeCertificate } from "../test/fakeClient";
import { Listener } from "./Listener";

// The owner's network listener settings against the fake client: the state, the setting and
// its refusals, and the certificate chosen in the desktop's dialogs.

afterEach(cleanup);

async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

async function open(c: FakeClient): Promise<HTMLElement> {
  render(<Listener client={c} onClose={() => {}} />);
  await flush();
  return screen.getByRole("dialog", { name: "Network access" });
}

function field(d: HTMLElement, label: string): HTMLInputElement {
  return within(d).getByLabelText(label) as HTMLInputElement;
}

describe("the network access view", () => {
  it("opens from the navigation and the palette, shows the listener off with its message, and closes", async () => {
    const c = new FakeClient();
    render(<App client={c} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Network access" }));
    await flush();
    const d = screen.getByRole("dialog", { name: "Network access" });
    expect(c.calls).toContain("listener");
    const state = within(d).getByRole("status");
    expect(state.textContent).toContain("Off");
    expect(state.textContent).toContain("The network listener is turned off.");
    expect(d.textContent).toContain("No certificate installed");
    expect(field(d, "Enabled").checked).toBe(false);
    expect(field(d, "Address").value).toBe("");
    expect(field(d, "Name").value).toBe("");
    fireEvent.click(within(d).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog", { name: "Network access" })).toBeNull();

    fireEvent.keyDown(window, { key: "p", ctrlKey: true, shiftKey: true });
    await flush();
    fireEvent.click(screen.getByText("Set up network access"));
    await flush();
    expect(screen.getByRole("dialog", { name: "Network access" })).toBeTruthy();
  });

  it("shows where it serves and the installed certificate's names and expiry", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" }, makeCertificate({ names: ["home.adanext.si", "ada.home.adanext.si"] }));
    const d = await open(c);
    expect(within(d).getByRole("status").textContent).toBe("Serving at https://home.adanext.si:8443");
    expect(field(d, "Enabled").checked).toBe(true);
    expect(field(d, "Address").value).toBe("192.0.2.20:8443");
    expect(field(d, "Name").value).toBe("home.adanext.si");
    const cert = within(d).getByRole("region", { name: "Certificate" });
    expect(cert.textContent).toContain("home.adanext.si, ada.home.adanext.si");
    expect(cert.querySelector("time")?.getAttribute("datetime")).toBe("2027-02-01T00:00:00Z");
    expect(cert.textContent).not.toContain("No certificate installed");
  });

  it("saves the setting and shows the status adad answers", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: false, address: "", name: "" }, makeCertificate());
    const d = await open(c);
    fireEvent.click(field(d, "Enabled"));
    fireEvent.change(field(d, "Address"), { target: { value: " 192.0.2.20:443 " } });
    fireEvent.change(field(d, "Name"), { target: { value: "home.adanext.si" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save" }));
    await flush();
    expect(c.args.setListener).toEqual([[{ enabled: true, address: "192.0.2.20:443", name: "home.adanext.si" }]]);
    expect(within(d).getByRole("status").textContent).toBe("Serving at https://home.adanext.si");
    expect(d.querySelector(".error")).toBeNull();
  });

  it("shows adad's message when it refuses the setting, and keeps what was typed", async () => {
    const c = new FakeClient();
    c.refuse.setListener = new AdaError("address_invalid", 400, "the address must be one of this computer's IP addresses with a port, such as 192.168.1.20:8443");
    const d = await open(c);
    fireEvent.click(field(d, "Enabled"));
    fireEvent.change(field(d, "Address"), { target: { value: "0.0.0.0:8443" } });
    fireEvent.change(field(d, "Name"), { target: { value: "home.adanext.si" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save" }));
    await flush();
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("address_invalid");
    expect(error.textContent).toContain("such as 192.168.1.20:8443");
    expect(field(d, "Address").value).toBe("0.0.0.0:8443");
    expect(field(d, "Enabled").checked).toBe(true);
    expect(within(d).getByRole("status").textContent).toContain("Off");
  });

  it("installs a certificate chosen in the dialogs and keeps unsaved fields", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" }, null);
    const d = await open(c);
    expect(within(d).getByRole("status").textContent).toContain("Install a certificate and its key");
    fireEvent.change(field(d, "Name"), { target: { value: "ada.adanext.si" } });
    fireEvent.click(within(d).getByRole("button", { name: "Choose certificate and key…" }));
    await flush();
    expect(c.calls).toContain("installListenerCertificate");
    expect(within(d).getByRole("status").textContent).toBe("Serving at https://home.adanext.si:8443");
    expect(within(d).getByRole("region", { name: "Certificate" }).querySelector("time")?.getAttribute("datetime")).toBe("2027-02-01T00:00:00Z");
    expect(field(d, "Name").value).toBe("ada.adanext.si");
  });

  it("changes nothing when the owner cancels a dialog", async () => {
    const c = new FakeClient();
    c.chosenCertificate = null;
    const d = await open(c);
    fireEvent.click(within(d).getByRole("button", { name: "Choose certificate and key…" }));
    await flush();
    expect(c.calls).toContain("installListenerCertificate");
    expect(d.textContent).toContain("No certificate installed");
    expect(d.querySelector(".error")).toBeNull();
  });

  it("shows adad's message when it refuses a certificate", async () => {
    const c = new FakeClient();
    c.refuse.installListenerCertificate = new AdaError("certificate_name", 400, "the certificate does not name home.adanext.si");
    const d = await open(c);
    fireEvent.click(within(d).getByRole("button", { name: "Choose certificate and key…" }));
    await flush();
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("certificate_name");
    expect(error.textContent).toContain("the certificate does not name home.adanext.si");
  });

  it("stays busy while a change is in flight", async () => {
    const c = new FakeClient();
    let answer: () => void = () => {};
    const pending = new Promise<void>((resolve) => (answer = resolve));
    const set = c.setListener.bind(c);
    c.setListener = async (setting) => {
      await pending;
      return set(setting);
    };
    const d = await open(c);
    fireEvent.click(within(d).getByRole("button", { name: "Save" }));
    await flush();
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect((within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement).disabled).toBe(true);
    for (const label of ["Enabled", "Address", "Name"]) expect(field(d, label).disabled).toBe(true);
    await act(async () => answer());
    await flush();
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    for (const label of ["Enabled", "Address", "Name"]) expect(field(d, label).disabled).toBe(false);

    let chosen: () => void = () => {};
    const choosing = new Promise<void>((resolve) => (chosen = resolve));
    const install = c.installListenerCertificate.bind(c);
    c.installListenerCertificate = async () => {
      await choosing;
      return install();
    };
    fireEvent.click(within(d).getByRole("button", { name: "Choose certificate and key…" }));
    await flush();
    for (const label of ["Enabled", "Address", "Name"]) expect(field(d, label).disabled).toBe(true);
    await act(async () => chosen());
    await flush();
    for (const label of ["Enabled", "Address", "Name"]) expect(field(d, label).disabled).toBe(false);
  });

  it("allows no change until the first read has answered", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" }, null);
    let answerFirst: () => void = () => {};
    const first = new Promise<void>((resolve) => (answerFirst = resolve));
    const read = c.listener.bind(c);
    c.listener = async () => {
      await first;
      return read();
    };
    render(<Listener client={c} onClose={() => {}} />);
    await flush();
    const d = screen.getByRole("dialog", { name: "Network access" });
    const save = within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    const choose = within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(choose.disabled).toBe(true);
    fireEvent.click(choose);
    fireEvent.click(save);
    await flush();
    expect(c.calls).not.toContain("installListenerCertificate");
    expect(c.calls).not.toContain("setListener");
    await act(async () => answerFirst());
    await flush();
    expect(field(d, "Address").value).toBe("192.0.2.20:8443");
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    expect((within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("explains that the name must resolve to the address, the certificate comes from files and nothing is served as plain HTTP", async () => {
    const d = await open(new FakeClient());
    const note = within(d).getByRole("note");
    expect(note.textContent).toContain("home.adanext.si");
    expect(note.textContent).toContain("must resolve to the address");
    expect(note.textContent).toContain("from files");
    expect(note.textContent).toContain("never serves plain HTTP");
  });

  it("shows why the status cannot be read", async () => {
    const c = new FakeClient();
    c.refuse.listener = new AdaError("listener_unavailable", 503, "the network listener is not available in this coordinator");
    const d = await open(c);
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("listener_unavailable");
    expect(within(d).queryByRole("status", { name: "Loading the listener" })).toBeNull();
  });

  it("ignores a read that answers after a newer one started: a late failure never hides the retry's result", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" }, makeCertificate());
    const read = c.listener.bind(c);
    let failFirst: (e: Error) => void = () => {};
    let answerRetry: () => void = () => {};
    let calls = 0;
    c.listener = () => {
      calls++;
      if (calls === 1) return new Promise((_, reject) => (failFirst = reject));
      return new Promise((resolve) => (answerRetry = () => resolve(read())));
    };
    // Strict mode runs the opening read twice, so the second starts while the first is pending.
    render(
      <StrictMode>
        <Listener client={c} onClose={() => {}} />
      </StrictMode>,
    );
    await flush();
    expect(calls).toBe(2);
    const d = screen.getByRole("dialog", { name: "Network access" });
    await act(async () => failFirst(new AdaError("listener_unavailable", 503, "the network listener is not available")));
    await flush();
    await act(async () => answerRetry());
    await flush();
    expect(d.querySelector(".error")).toBeNull();
    expect(within(d).getByRole("status").textContent).toBe("Serving at https://home.adanext.si:8443");
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    expect((within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("reads the status again after a failed first read, and only then allows changes", async () => {
    const c = new FakeClient();
    c.listenerStatus = listenerStatus({ enabled: true, address: "192.0.2.20:8443", name: "home.adanext.si" }, makeCertificate());
    c.refuse.listener = new AdaError("listener_unavailable", 503, "the network listener is not available in this coordinator");
    const d = await open(c);
    expect((d.querySelector(".error") as HTMLElement).textContent).toContain("listener_unavailable");
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect((within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement).disabled).toBe(true);
    delete c.refuse.listener;
    fireEvent.click(within(d).getByRole("button", { name: "Try again" }));
    await flush();
    expect(d.querySelector(".error")).toBeNull();
    expect(within(d).queryByRole("button", { name: "Try again" })).toBeNull();
    expect(within(d).getByRole("status").textContent).toBe("Serving at https://home.adanext.si:8443");
    expect(field(d, "Address").value).toBe("192.0.2.20:8443");
    expect((within(d).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    expect((within(d).getByRole("button", { name: "Choose certificate and key…" }) as HTMLButtonElement).disabled).toBe(false);
  });
});
