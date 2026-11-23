import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { AdaError } from "../client/api";
import { FakeClient, makeAccount } from "../test/fakeClient";
import { Accounts, groupCode } from "./Accounts";

// The owner's accounts view against the fake client: each action, its confirmation where it
// ends something, and adad's refusals explained.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

function client(): FakeClient {
  const c = new FakeClient();
  c.accountList = [makeAccount({ id: "local", name: "owner", owner: true, credentials: 2, sessions: 1, password_allowed: false }), makeAccount({ id: "u2", name: "sam", credentials: 1, sessions: 1 })];
  c.sessionList = [
    { id: "s1", account: "local", created: "2026-11-20T08:00:00Z", last_used: "2026-11-20T09:00:00Z", address: "192.0.2.10" },
    { id: "s2", account: "u2", created: "2026-11-21T08:00:00Z", last_used: "2026-11-21T09:30:00Z", address: "192.0.2.20" },
  ];
  return c;
}

async function open(c: FakeClient): Promise<HTMLElement> {
  render(<Accounts client={c} onClose={() => {}} />);
  await flush();
  return screen.getByRole("dialog", { name: "Accounts" });
}

function row(d: HTMLElement, id: string): HTMLElement {
  return d.querySelector(`[data-account="${id}"]`) as HTMLElement;
}

describe("the accounts view", () => {
  it("opens from the navigation and the palette, lists every account and session, and closes", async () => {
    const c = client();
    render(<App client={c} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    await flush();
    const d = screen.getByRole("dialog", { name: "Accounts" });
    expect(c.calls).toContain("listAccounts");
    expect(c.calls).toContain("listSessions");
    const owner = row(d, "local");
    expect(within(owner).getByText("owner", { selector: ".tag" })).toBeTruthy();
    expect(owner.textContent).toContain("2");
    // The owner can be recovered and given a code, never disabled or removed.
    expect(within(owner).getByRole("button", { name: "Recover owner" })).toBeTruthy();
    expect(within(owner).queryByRole("button", { name: /Disable|Remove/ })).toBeNull();
    expect(within(row(d, "u2")).getByRole("button", { name: "Disable sam" })).toBeTruthy();
    expect(within(row(d, "u2")).getByRole("button", { name: "Remove sam" })).toBeTruthy();
    const session = d.querySelector('[data-session="s2"]') as HTMLElement;
    expect(session.textContent).toContain("sam");
    expect(session.textContent).toContain("192.0.2.20");
    expect(Array.from(session.querySelectorAll("time")).map((t) => t.getAttribute("datetime"))).toEqual(["2026-11-21T08:00:00Z", "2026-11-21T09:30:00Z"]);
    fireEvent.click(within(d).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog", { name: "Accounts" })).toBeNull();

    fireEvent.keyDown(window, { key: "p", ctrlKey: true, shiftKey: true });
    await flush();
    fireEvent.click(screen.getByText("Manage accounts and sessions"));
    await flush();
    expect(screen.getByRole("dialog", { name: "Accounts" })).toBeTruthy();
  });

  it("creates an account and shows its code once, grouped, with its expiry, a copy button and where to enter it", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const c = client();
    const d = await open(c);
    const create = within(d).getByRole("button", { name: "Create the account" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "  robin " } });
    fireEvent.click(within(d).getByLabelText("Allow a password as well as a passkey"));
    fireEvent.click(create);
    await flush();
    expect(c.args.createAccount).toEqual([[{ name: "robin", password_allowed: true }]]);
    const panel = within(d).getByRole("group", { name: "Enrollment code" });
    expect(within(panel).getByText("ABCDE-12345-FGHJK-67891")).toBeTruthy();
    expect(panel.querySelector("time")?.getAttribute("datetime")).toBe("2026-11-24T10:00:00Z");
    expect(panel.textContent).toContain("shown only this once");
    expect(panel.textContent).toContain("on their own computer at Ada's web address");
    fireEvent.click(within(panel).getByRole("button", { name: "Copy the code" }));
    await flush();
    expect(writeText).toHaveBeenCalledWith("ABCDE-12345-FGHJK-67891");
    expect(within(panel).getByRole("status").textContent).toBe("Copied.");
    // The list is read back with the new account; the form is empty again.
    expect(row(d, "u3").textContent).toContain("robin");
    expect((within(d).getByLabelText("Name") as HTMLInputElement).value).toBe("");
    fireEvent.click(within(panel).getByRole("button", { name: "Done" }));
    expect(within(d).queryByRole("group", { name: "Enrollment code" })).toBeNull();
    expect(d.textContent).not.toContain("ABCDE-12345-FGHJK-67891");
  });

  it("says so when the code cannot be copied", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: async () => Promise.reject(new Error("denied")) } });
    const c = client();
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "New code for sam" }));
    await flush();
    fireEvent.click(within(d).getByRole("button", { name: "Copy the code" }));
    await flush();
    expect(within(d).getByRole("status").textContent).toContain("Could not copy");
  });

  it("forgets that a code was copied when another code replaces it", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: vi.fn(async () => {}) } });
    const c = client();
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "New code for sam" }));
    await flush();
    const first = within(within(d).getByRole("group", { name: "Enrollment code" })).getByText(/^[0-9A-Z]{5}-/).textContent;
    fireEvent.click(within(d).getByRole("button", { name: "Copy the code" }));
    await flush();
    expect(within(d).getByRole("status").textContent).toBe("Copied.");
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "New code for sam" }));
    await flush();
    const second = within(within(d).getByRole("group", { name: "Enrollment code" })).getByText(/^[0-9A-Z]{5}-/).textContent;
    expect(second).not.toBe(first);
    expect(within(d).getByRole("status").textContent).toBe("");
  });

  it("keeps the latest read when an earlier one answers after it, the first load included", async () => {
    const c = client();
    const stale = { accounts: c.accountList, sessions: c.sessionList };
    let answerFirst: () => void = () => {};
    const first = new Promise<void>((resolve) => (answerFirst = resolve));
    const list = c.listAccounts.bind(c);
    const sessions = c.listSessions.bind(c);
    let reads = 0;
    c.listAccounts = async () => {
      if (++reads === 1) {
        await first;
        return stale.accounts;
      }
      return list();
    };
    c.listSessions = async () => {
      if (reads === 1) {
        await first;
        return stale.sessions;
      }
      return sessions();
    };
    render(<Accounts client={c} onClose={() => {}} />);
    await flush();
    const d = screen.getByRole("dialog", { name: "Accounts" });
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "robin" } });
    fireEvent.click(within(d).getByRole("button", { name: "Create the account" }));
    await flush();
    expect(row(d, "u3").textContent).toContain("robin");
    await act(async () => answerFirst());
    await flush();
    expect(row(d, "u3")?.textContent).toContain("robin");
  });

  it("stays busy until the read after a change has answered", async () => {
    const c = client();
    const d = await open(c);
    let answer: () => void = () => {};
    const pending = new Promise<void>((resolve) => (answer = resolve));
    const list = c.listAccounts.bind(c);
    c.listAccounts = async () => {
      await pending;
      return list();
    };
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Disable sam" }));
    await flush();
    expect((within(row(d, "u2")).getByRole("button", { name: "Disable sam" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => answer());
    await flush();
    expect((within(row(d, "u2")).getByRole("button", { name: "Enable sam" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("issues a new code for an account", async () => {
    const c = client();
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "New code for sam" }));
    await flush();
    expect(c.args.issueEnrollmentCode).toEqual([["u2"]]);
    const panel = within(d).getByRole("group", { name: "Enrollment code" });
    expect(panel.textContent).toContain("sam");
    expect(within(panel).getByText("ABCDE-12345-FGHJK-67891")).toBeTruthy();
  });

  it("explains code_shown: a code is shown once, so a new one must be issued", async () => {
    const c = client();
    c.refuse.issueEnrollmentCode = new AdaError("code_shown", 409, "the code was already shown");
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "New code for sam" }));
    await flush();
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("code_shown");
    expect(error.textContent).toContain("issue a new code");
    expect(within(d).queryByRole("group", { name: "Enrollment code" })).toBeNull();
  });

  it("recovers an account only once confirmed, ending its sessions and showing the new code", async () => {
    const c = client();
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Recover sam" }));
    const ask = within(d).getByRole("group", { name: "Confirm" });
    expect(ask.textContent).toContain("ends every session of the account, clears its sign-in throttling and gives a new enrollment code");
    // Focus starts on the way out.
    expect(document.activeElement).toBe(within(ask).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(ask).getByRole("button", { name: "Cancel" }));
    expect(c.calls).not.toContain("recoverAccount");
    expect(within(d).queryByRole("group", { name: "Confirm" })).toBeNull();

    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Recover sam" }));
    fireEvent.click(within(d).getByRole("button", { name: "Yes, recover sam" }));
    await flush();
    expect(c.args.recoverAccount).toEqual([["u2"]]);
    expect(within(d).getByRole("group", { name: "Enrollment code" }).textContent).toContain("every session ended and the sign-in throttling cleared");
    expect(d.querySelector('[data-session="s2"]')).toBeNull();
    expect(d.querySelector('[data-session="s1"]')).not.toBeNull();
  });

  it("disables and enables an account, and allows a password", async () => {
    const c = client();
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Disable sam" }));
    await flush();
    expect(c.args.updateAccount).toEqual([["u2", { disabled: true }]]);
    expect(within(row(d, "u2")).getByText("disabled")).toBeTruthy();
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Enable sam" }));
    await flush();
    expect(c.args.updateAccount?.at(-1)).toEqual(["u2", { disabled: false }]);
    expect(within(row(d, "u2")).queryByText("disabled")).toBeNull();

    const box = within(row(d, "u2")).getByRole("checkbox", { name: "Allow a password for sam" }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    await flush();
    expect(c.args.updateAccount?.at(-1)).toEqual(["u2", { password_allowed: true }]);
    expect((within(row(d, "u2")).getByRole("checkbox", { name: "Allow a password for sam" }) as HTMLInputElement).checked).toBe(true);
  });

  it("removes an account once confirmed, and explains account_in_use: disable it instead", async () => {
    const c = client();
    c.refuse.removeAccount = new AdaError("account_in_use", 409, "the account has history");
    const d = await open(c);
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Remove sam" }));
    expect(within(d).getByRole("group", { name: "Confirm" }).textContent).toContain("cannot be undone");
    fireEvent.click(within(d).getByRole("button", { name: "Yes, remove sam" }));
    await flush();
    expect(c.args.removeAccount).toEqual([["u2"]]);
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("account_in_use");
    expect(error.textContent).toContain("disable it instead");
    expect(row(d, "u2")).not.toBeNull();
    expect(within(d).queryByRole("group", { name: "Confirm" })).toBeNull();

    delete c.refuse.removeAccount;
    fireEvent.click(within(row(d, "u2")).getByRole("button", { name: "Remove sam" }));
    fireEvent.click(within(d).getByRole("button", { name: "Yes, remove sam" }));
    await flush();
    expect(row(d, "u2")).toBeNull();
    expect(d.querySelector(".error")).toBeNull();
  });

  it("explains owner_account when adad refuses to disable the owner", async () => {
    const c = client();
    // A listing that did not mark the owner: adad still refuses.
    c.accountList = [makeAccount({ id: "local", name: "owner" })];
    c.refuse.updateAccount = new AdaError("owner_account", 409, "the owner account");
    const d = await open(c);
    fireEvent.click(within(row(d, "local")).getByRole("button", { name: "Disable owner" }));
    await flush();
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("owner_account");
    expect(error.textContent).toContain("cannot be disabled or removed");
  });

  it("explains local_only when the accounts cannot be read from here", async () => {
    const c = client();
    c.refuse.listAccounts = new AdaError("local_only", 403, "owner channel only");
    const d = await open(c);
    const error = d.querySelector(".error") as HTMLElement;
    expect(error.textContent).toContain("local_only");
    expect(error.textContent).toContain("only in the Ada desktop app");
    expect(within(d).queryByRole("status", { name: "Loading accounts" })).toBeNull();
  });

  it("revokes a session once confirmed, and says when no one is signed in", async () => {
    const c = client();
    const d = await open(c);
    fireEvent.click(within(d).getByRole("button", { name: "Revoke the session of sam from 192.0.2.20" }));
    expect(within(d).getByRole("group", { name: "Confirm" }).textContent).toContain("signed out at once");
    fireEvent.click(within(d).getByRole("button", { name: "Yes, end the session" }));
    await flush();
    expect(c.args.revokeSession).toEqual([["s2"]]);
    expect(d.querySelector('[data-session="s2"]')).toBeNull();
    fireEvent.click(within(d).getByRole("button", { name: "Revoke the session of owner from 192.0.2.10" }));
    fireEvent.click(within(d).getByRole("button", { name: "Yes, end the session" }));
    await flush();
    expect(within(d).getByText("No one is signed in through the web.")).toBeTruthy();
  });

  it("groups a 20-character code in fives and leaves anything else as it came", () => {
    expect(groupCode("abcde12345fghjk67890")).toBe("ABCDE-12345-FGHJK-67890");
    expect(groupCode("ABCDE-12345-FGHJK-67890")).toBe("ABCDE-12345-FGHJK-67890");
    expect(groupCode("short")).toBe("short");
  });
});
