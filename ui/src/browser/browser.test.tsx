import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AdaError, type BridgeApi, type SignedInAccount } from "../client/api";
import { createBridgeClient } from "../client/adapters/bridge";
import { createHttpsClient } from "../client/adapters/https";
import { toBase64url } from "../client/adapters/webauthn";
import { FakeAdad } from "../test/fakeAdad";
import { FakeClient, makeTask } from "../test/fakeClient";
import { Root } from "./Gate";
import { reloadToStart } from "./reload";

// The reload ends the page; here it is only counted.
vi.mock("./reload", () => ({ reloadToStart: vi.fn() }));
const reload = vi.mocked(reloadToStart);
beforeEach(() => reload.mockClear());

// The browser shell through the HTTPS adapter and the fake adad: sign-in with a passkey or a
// password, enrollment with a code, the own account's sessions and credentials, Leave, and a
// workspace without the desktop's controls and without any experience request.

const sam: SignedInAccount = { id: "u1", name: "sam", owner: false, password_allowed: true };
const kim: SignedInAccount = { id: "u2", name: "kim", owner: false, password_allowed: false };

function adad(): FakeAdad {
  const a = new FakeAdad([makeTask({ id: "t1", project: "p1", objective: "sam's task", state: "running", attempt: { id: "a1", state: "running" } })]);
  a.projectList = [
    { id: "p1", name: "sam's repo", role: "owner", created: "" },
    { id: "p2", name: "sam's empty project", role: "owner", created: "" },
  ];
  a.passwords = { sam: { account: sam, password: "correct horse battery" }, kim: { account: kim, password: "correct horse battery" } };
  return a;
}

function browser(a: FakeAdad) {
  return render(<Root client={createHttpsClient({ fetch: a.fetch, EventSource: a.EventSource })} />);
}

function stubCredentials(credentials: Partial<CredentialsContainer>): void {
  Object.defineProperty(navigator, "credentials", { value: credentials, configurable: true });
}

afterEach(() => {
  cleanup();
  Object.defineProperty(navigator, "credentials", { value: undefined, configurable: true });
  delete (window as { ada?: BridgeApi }).ada;
});

async function signInWithPassword(a: FakeAdad, name = "sam") {
  browser(a);
  fireEvent.change(await screen.findByLabelText("Account name"), { target: { value: name } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "correct horse battery" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in with the password" }));
}

describe("signing in from a browser", () => {
  it("asks for the account first and shows the sign-in page without a session", async () => {
    const a = adad();
    browser(a);
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeTruthy();
    expect(a.requests.map((r) => `${r.method} ${r.url}`)).toEqual(["GET /v1/account"]);
    expect(screen.getByRole("button", { name: "Sign in with a passkey" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Enroll with a code" })).toBeTruthy();
  });

  it("signs in with a password and opens the workspace on the account's projects, the empty one included", async () => {
    const a = adad();
    await signInWithPassword(a);
    const nav = await screen.findByRole("navigation", { name: "Projects and tasks" });
    expect(await within(nav).findByText("sam's empty project")).toBeTruthy();
    expect(within(nav).getByText("sam's task")).toBeTruthy();
    expect(JSON.parse(a.requests.find((r) => r.url === "/v1/session/password")!.body)).toEqual({ account: "sam", password: "correct horse battery" });
    expect(screen.getByText("Signed in as")).toBeTruthy();
  });

  it("offers the password form to every account and shows adad's refusal where the account allows none", async () => {
    const a = adad();
    await signInWithPassword(a, "kim");
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("login_failed");
    expect(screen.getByRole("heading", { name: "Sign in" })).toBeTruthy();
  });

  it("signs in with a passkey through the browser's authenticator", async () => {
    const a = adad();
    a.handles[toBase64url(new Uint8Array([7]).buffer)] = sam;
    stubCredentials({
      get: async () => ({ id: "c", rawId: new Uint8Array([1]).buffer, type: "public-key", response: { clientDataJSON: new Uint8Array([2]).buffer, authenticatorData: new Uint8Array([3]).buffer, signature: new Uint8Array([4]).buffer, userHandle: new Uint8Array([7]).buffer } }) as unknown as Credential,
    });
    browser(a);
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByRole("navigation", { name: "Projects and tasks" })).toBeTruthy();
    expect(a.requests.map((r) => r.url)).toContain("/v1/session/passkey");
  });
});

describe("enrolling with a code", () => {
  async function enroll(a: FakeAdad, code: string) {
    browser(a);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: code } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  }

  it("takes the code, names the account, and offers a password only where the account allows one", async () => {
    const a = adad();
    a.enrollCodes = { KIMCODE: kim };
    await enroll(a, "KIMCODE");
    expect((await screen.findByText(/This code is for the account/)).textContent).toContain("account kim.");
    expect(screen.getByRole("button", { name: "Add a passkey and sign in" })).toBeTruthy();
    expect(screen.queryByLabelText("New password")).toBeNull();
  });

  it("enrolls a password and signs in", async () => {
    const a = adad();
    a.enrollCodes = { SAMCODE: sam };
    await enroll(a, "SAMCODE");
    fireEvent.change(await screen.findByLabelText("New password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Set the password and sign in" }));
    expect(await screen.findByRole("navigation", { name: "Projects and tasks" })).toBeTruthy();
    expect(JSON.parse(a.requests.find((r) => r.url === "/v1/enrollment/password")!.body)).toEqual({ code: "SAMCODE", password: "a long enough secret" });
  });

  it("enrolls a passkey through the browser's authenticator and signs in", async () => {
    const a = adad();
    a.enrollCodes = { KIMCODE: kim };
    stubCredentials({ create: async () => ({ id: "n", rawId: new Uint8Array([1]).buffer, type: "public-key", response: { clientDataJSON: new Uint8Array([2]).buffer, attestationObject: new Uint8Array([3]).buffer } }) as unknown as Credential });
    await enroll(a, "KIMCODE");
    fireEvent.change(await screen.findByLabelText("Name this passkey"), { target: { value: "Work laptop" } });
    fireEvent.click(screen.getByRole("button", { name: "Add a passkey and sign in" }));
    expect(await screen.findByRole("navigation", { name: "Projects and tasks" })).toBeTruthy();
    expect(JSON.parse(a.requests.find((r) => r.url === "/v1/enrollment/passkey")!.body)).toMatchObject({ code: "KIMCODE", ceremony: "enroll-KIMCODE", name: "Work laptop" });
  });

  it("shows a code adad refuses", async () => {
    await enroll(adad(), "WRONG");
    expect((await screen.findByRole("alert")).textContent).toContain("code_invalid");
  });
});

describe("the signed-in browser", () => {
  it("leaves out the desktop's controls and makes no experience request", async () => {
    const a = adad();
    await signInWithPassword(a);
    fireEvent.click(await screen.findByText("sam's task"));
    await screen.findByRole("tablist", { name: "Work area" });
    for (const name of ["Import a repository", "New task", "Accounts", "Network access", "Quit Ada and stop work", "Start"]) expect(screen.queryByRole("button", { name })).toBeNull();
    expect(screen.queryByRole("region", { name: "Experience" })).toBeNull();
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(a.requests.filter((r) => r.url.startsWith("/v1/experience"))).toEqual([]);
  });

  it("the desktop, by contrast, reads the experience card through the bridge", async () => {
    const a = adad();
    (window as { ada?: BridgeApi }).ada = a.bridge();
    render(<Root client={createBridgeClient()} />);
    await waitFor(() => expect(a.requests.some((r) => r.url === "/v1/experience")).toBe(true));
    expect(a.requests.some((r) => r.url === "/v1/account")).toBe(false);
    expect(screen.getByRole("button", { name: "Quit Ada and stop work" })).toBeTruthy();
  });

  it("Leave signs out, stops nothing, says so, retires the workspace and reloads once; a double click sends one logout", async () => {
    const a = adad();
    await signInWithPassword(a);
    const leave = await screen.findByRole("button", { name: "Leave" });
    expect(screen.getByText(/your tasks keep running/)).toBeTruthy();
    fireEvent.click(leave);
    fireEvent.click(leave);
    expect(await screen.findByText("Signing in again…")).toBeTruthy();
    expect(screen.queryByText("sam's task")).toBeNull();
    expect(screen.queryAllByRole("button")).toEqual([]);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(a.signedIn).toBeNull();
    expect(a.requests.filter((r) => r.method === "POST").map((r) => r.url)).toEqual(["/v1/session/password", "/v1/session/logout"]);
  });

  it("shows the account's sessions with this browser marked and ends another, and lists its credentials", async () => {
    const a = adad();
    a.ownSessionList = [
      { id: "s1", created: "2026-11-24T10:00:00Z", last_used: "2026-11-24T11:00:00Z", address: "192.0.2.10", current: true },
      { id: "s2", created: "2026-11-23T10:00:00Z", last_used: "2026-11-23T11:00:00Z", address: "192.0.2.20", current: false },
    ];
    a.credentialList = [
      { id: "c1", kind: "passkey", name: "Work laptop", created: "2026-11-24T10:00:00Z", last_used: "" },
      { id: "c2", kind: "password", name: "Password", created: "2026-11-24T10:00:00Z", last_used: "" },
    ];
    await signInWithPassword(a);
    fireEvent.click(await screen.findByRole("button", { name: "Your account" }));
    const dialog = await screen.findByRole("dialog", { name: /Your account/ });
    const current = await within(dialog).findByText("this browser");
    expect(current.closest("tr")!.getAttribute("data-session")).toBe("s1");
    expect(within(dialog).getByText("Work laptop")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "End the session from 192.0.2.20" }));
    await waitFor(() => expect(within(dialog).queryByText("192.0.2.20")).toBeNull());
    expect(a.requests.some((r) => r.method === "DELETE" && r.url === "/v1/account/sessions/s2")).toBe(true);
    expect(screen.getByRole("navigation", { name: "Projects and tasks" })).toBeTruthy();
  });

  it("removing a credential signs out everywhere, retires the workspace and reloads once", async () => {
    const a = adad();
    a.ownSessionList = [{ id: "s1", created: "", last_used: "", address: "192.0.2.10", current: true }];
    a.credentialList = [
      { id: "c1", kind: "passkey", name: "Work laptop", created: "", last_used: "" },
      { id: "c2", kind: "password", name: "Password", created: "", last_used: "" },
    ];
    await signInWithPassword(a);
    fireEvent.click(await screen.findByRole("button", { name: "Your account" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove Work laptop" }));
    expect(await screen.findByText("Signing in again…")).toBeTruthy();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(a.requests.some((r) => r.method === "DELETE" && r.url === "/v1/account/credentials/c1")).toBe(true);
  });

  it("signing this browser out from the account page retires the workspace and reloads once", async () => {
    const a = adad();
    a.ownSessionList = [{ id: "s1", created: "", last_used: "", address: "192.0.2.10", current: true }];
    await signInWithPassword(a);
    fireEvent.click(await screen.findByRole("button", { name: "Your account" }));
    fireEvent.click(await screen.findByRole("button", { name: "Sign out this browser" }));
    expect(await screen.findByText("Signing in again…")).toBeTruthy();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("keeps the last credential the account can use: a passkey beside a password the account no longer allows", async () => {
    const client = new FakeClient([], "network");
    client.signedIn = { ...sam, password_allowed: false };
    client.credentialList = [
      { id: "c1", kind: "passkey", name: "Laptop", created: "", last_used: "" },
      { id: "c2", kind: "password", name: "Password", created: "", last_used: "" },
    ];
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Your account" }));
    expect(((await screen.findByRole("button", { name: "Remove Laptop" })) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Remove Password" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("offers a password only as the server counts one: 12 code points and at most 1,024 bytes", async () => {
    const client = new FakeClient([], "network");
    client.enrollCodes = { FIRST: sam };
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "FIRST" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    const field = await screen.findByLabelText("New password");
    const button = () => screen.getByRole("button", { name: "Set the password and sign in" }) as HTMLButtonElement;
    const key = "\u{1F511}";
    for (const [value, allowed] of [[key.repeat(11), false], [key.repeat(12), true], ["a".repeat(1025), false], ["a".repeat(1024), true], ["é".repeat(513), false]] as const) {
      fireEvent.change(field, { target: { value } });
      expect(button().disabled, `${[...value].length} code points`).toBe(!allowed);
    }
    fireEvent.change(field, { target: { value: key.repeat(11) } });
    fireEvent.submit(button().closest("form")!);
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(client.calls).not.toContain("enrollPassword");
  });
});

describe("pending submissions", () => {
  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((r) => (resolve = r));
    return { promise, resolve };
  }

  it("finishes enrollment with the code submitted, whatever the field holds after, and names that code's account", async () => {
    const client = new FakeClient([], "network");
    client.enrollCodes = { FIRST: sam, OTHER: kim };
    const options = deferred<void>();
    const real = client.enrollmentOptions.bind(client);
    client.enrollmentOptions = async (code) => {
      await options.promise;
      return real(code);
    };
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "FIRST" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    const field = screen.getByLabelText("Enrollment code") as HTMLInputElement;
    expect(field.disabled).toBe(true);
    fireEvent.change(field, { target: { value: "OTHER" } });
    await act(async () => options.resolve());
    expect((await screen.findByText(/This code is for the account/)).textContent).toContain("account sam.");
    expect((screen.getByLabelText("Enrollment code") as HTMLInputElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Set the password and sign in" }));
    await screen.findByRole("navigation", { name: "Projects and tasks" });
    expect(client.args.enrollPassword).toEqual([["FIRST", "a long enough secret"]]);
  });

  it("going back to change the code starts over", async () => {
    const client = new FakeClient([], "network");
    client.enrollCodes = { FIRST: sam, OTHER: kim };
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "FIRST" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    await screen.findByText(/This code is for the account/);
    fireEvent.click(screen.getByRole("button", { name: "Use another code" }));
    const field = screen.getByLabelText("Enrollment code") as HTMLInputElement;
    expect(field.disabled).toBe(false);
    expect(screen.queryByText(/This code is for the account/)).toBeNull();
    fireEvent.change(field, { target: { value: "OTHER" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect((await screen.findByText(/This code is for the account/)).textContent).toContain("account kim.");
  });

  it("keeps sign-in and enrollment from replacing each other while a submission or passkey prompt is pending", async () => {
    const client = new FakeClient([], "network");
    client.enrollCodes = { FIRST: sam };
    const login = deferred<void>();
    client.passkeyLogin = async () => {
      await login.promise;
      throw new AdaError("passkey_unavailable", 0, "cancelled");
    };
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect((screen.getByRole("button", { name: "Enroll with a code" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => login.resolve());
    expect((screen.getByRole("button", { name: "Enroll with a code" }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "FIRST" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    await screen.findByText(/This code is for the account/);
    const enroll = deferred<void>();
    const real = client.enrollPasskey.bind(client);
    client.enrollPasskey = async (...args) => {
      await enroll.promise;
      return real(...args);
    };
    fireEvent.click(screen.getByRole("button", { name: "Add a passkey and sign in" }));
    expect((screen.getByRole("button", { name: "Back to sign-in" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Use another code" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => enroll.resolve());
    expect(await screen.findByRole("navigation", { name: "Projects and tasks" })).toBeTruthy();
  });
});

describe("changing the enrollment code", () => {
  it("starts the second code's form empty", async () => {
    const client = new FakeClient([], "network");
    client.enrollCodes = { FIRST: sam, SECOND: { ...sam, id: "u3", name: "lee" } };
    client.refuse.enrollPassword = new AdaError("password_invalid", 400, "too short");
    render(<Root client={client} />);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "FIRST" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.change(await screen.findByLabelText("New password"), { target: { value: "a long enough secret" } });
    fireEvent.change(screen.getByLabelText("Name this passkey"), { target: { value: "Mine" } });
    fireEvent.click(screen.getByRole("button", { name: "Set the password and sign in" }));
    await screen.findByRole("alert");
    fireEvent.click(screen.getByRole("button", { name: "Use another code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "SECOND" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect((await screen.findByText(/This code is for the account/)).textContent).toContain("account lee.");
    expect((screen.getByLabelText("New password") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Name this passkey") as HTMLInputElement).value).toBe("Passkey");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("a session that ended or is not this page's reloads the page", () => {
  async function signedIn(a: FakeAdad) {
    a.requireSession = true;
    await signInWithPassword(a);
    await screen.findByText("sam's task");
  }
  const tick = () => act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 10));
  });

  it("a 401 from a read reloads once", async () => {
    const a = adad();
    await signedIn(a);
    a.signedIn = null;
    fireEvent.click(screen.getByText("sam's task"));
    await waitFor(() => expect(reload).toHaveBeenCalled());
    await tick();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("a 401 from a write reloads once", async () => {
    const a = adad();
    a.tasks = [makeTask({ id: "d1", project: "p1", objective: "sam's draft" })];
    await signInWithPassword(a);
    a.requireSession = true;
    fireEvent.click(await screen.findByText("sam's draft"));
    await tick();
    a.signedIn = null;
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await waitFor(() => expect(reload).toHaveBeenCalled());
    await tick();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(a.requests.some((r) => r.method === "POST" && r.url === "/v1/tasks/d1/ready")).toBe(true);
  });

  it("a 401 from the account page's action reloads once", async () => {
    const a = adad();
    a.ownSessionList = [
      { id: "s1", created: "", last_used: "", address: "192.0.2.10", current: true },
      { id: "s2", created: "", last_used: "", address: "192.0.2.20", current: false },
    ];
    await signedIn(a);
    fireEvent.click(screen.getByRole("button", { name: "Your account" }));
    const end = await screen.findByRole("button", { name: "End the session from 192.0.2.20" });
    a.signedIn = null;
    fireEvent.click(end);
    await waitFor(() => expect(reload).toHaveBeenCalled());
    await tick();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("a stream's refused open whose account check answers 401 reloads once", async () => {
    const a = adad();
    await signedIn(a);
    fireEvent.click(screen.getByText("sam's task"));
    await waitFor(() => expect(a.sources).toHaveLength(1));
    await tick();
    a.signedIn = null;
    await act(async () => {
      a.sources[0]!.fail();
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    await waitFor(() => expect(reload).toHaveBeenCalled());
    await tick();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(a.requests.some((r) => r.url === "/v1/account")).toBe(true);
  });

  it("a focus check answering another account reloads, and one answering 401 reloads", async () => {
    const a = adad();
    await signInWithPassword(a);
    await screen.findByText("sam's task");
    a.signedIn = kim;
    await act(async () => {
      fireEvent.focus(window);
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Signing in again…")).toBeTruthy();
    expect(screen.queryByText("sam's task")).toBeNull();
    expect(screen.queryAllByRole("button")).toEqual([]);
    reload.mockClear();
    cleanup();
    const b = adad();
    await signInWithPassword(b);
    await screen.findByText("sam's task");
    b.signedIn = null;
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("a focus check answering the same account changes nothing and keeps a draft; a failing one changes nothing", async () => {
    const a = adad();
    a.tasks = [makeTask({ id: "t2", project: "p1", objective: "sam's review", state: "awaiting_review", attempt: { id: "a2", state: "ended_ok" } })];
    await signInWithPassword(a);
    fireEvent.click(await screen.findByText("sam's review"));
    fireEvent.change(await screen.findByLabelText("Finding"), { target: { value: "a draft finding" } });
    const before = a.requests.length;
    await act(async () => {
      fireEvent.focus(window);
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    a.refusals["GET /v1/account"] = { status: 500, body: { error: "internal", message: "internal error" } };
    await act(async () => {
      fireEvent.focus(window);
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(reload).not.toHaveBeenCalled();
    expect((screen.getByLabelText("Finding") as HTMLTextAreaElement).value).toBe("a draft finding");
    expect(a.requests.slice(before).filter((r) => r.url === "/v1/account")).toHaveLength(2);
  });

  it("two focus events while a check is pending make one request", async () => {
    const a = adad();
    let release = () => {};
    let holding = false;
    const fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      if (holding && String(input) === "/v1/account") await new Promise<void>((r) => (release = r));
      return a.fetch(input, init);
    };
    render(<Root client={createHttpsClient({ fetch, EventSource: a.EventSource })} />);
    fireEvent.change(await screen.findByLabelText("Account name"), { target: { value: "sam" } });
    fireEvent.change(screen.getByLabelText("Password", { selector: "input" }), { target: { value: "correct horse battery" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in with the password" }));
    await screen.findByText("sam's task");
    holding = true;
    const before = a.requests.length;
    await act(async () => {
      fireEvent.focus(window);
      fireEvent.focus(window);
      document.dispatchEvent(new Event("visibilitychange"));
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    await act(async () => {
      release();
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(a.requests.slice(before).filter((r) => r.url === "/v1/account")).toHaveLength(1);
    expect(reload).not.toHaveBeenCalled();
  });

  it("sign-in and enrollment refusals show on the form and never reload", async () => {
    const a = adad();
    a.requireSession = true;
    await signInWithPassword(a, "kim");
    expect((await screen.findByRole("alert")).textContent).toContain("login_failed");
    cleanup();
    const b = adad();
    b.requireSession = true;
    b.enrollCodes = { SAMCODE: sam };
    b.refusals["POST /v1/enrollment/password"] = { status: 401, body: { error: "login_failed", message: "sign-in failed" } };
    browser(b);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll with a code" }));
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "SAMCODE" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.change(await screen.findByLabelText("New password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Set the password and sign in" }));
    expect((await screen.findByRole("alert")).textContent).toContain("login_failed");
    expect(reload).not.toHaveBeenCalled();
  });
});
