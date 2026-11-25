import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { AdaError } from "../client/api";
import { FakeClient, makeAccount } from "../test/fakeClient";
import { ProjectSettings } from "./ProjectSettings";

// The owner's membership administration and a project's default mode, on the desktop only.

afterEach(cleanup);

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function client(): FakeClient {
  const c = new FakeClient();
  c.projectList = [{ id: "p1", name: "repo", role: "owner", created: "", default_mode: "readonly" }];
  c.accountList = [makeAccount({ id: "local", name: "owner", owner: true }), makeAccount({ id: "u2", name: "kim" }), makeAccount({ id: "u3", name: "lee" })];
  c.memberList = { p1: [{ user: "local", name: "owner", role: "owner", added: "2026-11-01T00:00:00Z" }, { user: "u2", name: "kim", role: "member", added: "2026-11-02T00:00:00Z" }] };
  return c;
}

function open(c: FakeClient) {
  render(<ProjectSettings client={c} project={c.projectList[0]!} onClose={() => {}} onChanged={() => {}} />);
}

describe("project settings", () => {
  it("lists the members with their roles", async () => {
    const c = client();
    open(c);
    const table = await screen.findByRole("table", { name: "Members" });
    const rows = within(table).getAllByRole("row").filter((r) => r.getAttribute("data-member"));
    expect(rows.map((r) => r.getAttribute("data-member"))).toEqual(["local", "u2"]);
    expect(within(rows[1]!).getByText("member")).toBeTruthy();
    expect(c.args.members).toEqual([["p1"]]);
  });

  it("adds an account from the accounts list as a member or an owner", async () => {
    const c = client();
    open(c);
    await screen.findByRole("table", { name: "Members" });
    const account = screen.getByLabelText("Account") as HTMLSelectElement;
    // Only accounts not already members are offered.
    expect(Array.from(account.options).map((o) => o.value)).toEqual(["u3"]);
    fireEvent.change(account, { target: { value: "u3" } });
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "owner" } });
    fireEvent.click(screen.getByRole("button", { name: "Add to the project" }));
    await flush();
    expect(c.args.setMember).toEqual([["p1", "u3", "owner"]]);
    expect(within(screen.getByRole("table", { name: "Members" })).getByText("lee")).toBeTruthy();
  });

  it("changes a role only after a warning that it signs the account out everywhere", async () => {
    const c = client();
    open(c);
    await screen.findByRole("table", { name: "Members" });
    fireEvent.click(screen.getByRole("button", { name: "Make kim an owner" }));
    const confirm = screen.getByRole("group", { name: "Confirm" });
    expect(confirm.textContent).toContain("signs kim out everywhere");
    expect(c.calls).not.toContain("setMember");
    fireEvent.click(within(confirm).getByRole("button", { name: "Yes, change the role" }));
    await flush();
    expect(c.args.setMember).toEqual([["p1", "u2", "owner"]]);
  });

  it("removes a member only after a warning about their decisions and attempts, and shows last_owner", async () => {
    const c = client();
    open(c);
    await screen.findByRole("table", { name: "Members" });
    fireEvent.click(screen.getByRole("button", { name: "Remove kim" }));
    const confirm = screen.getByRole("group", { name: "Confirm" });
    // The reasons are the ones adad records on each attempt.
    expect(confirm.textContent).toContain("kim's attempts waiting for a decision are denied and cancelled with the reason “membership revoked”");
    expect(confirm.textContent).toContain("kim's other queued and running attempts in the project, tasks in verification included, are cancelled with the reason “actor removed from the project”");
    fireEvent.click(within(confirm).getByRole("button", { name: "Yes, remove kim" }));
    await flush();
    expect(c.args.removeMember).toEqual([["p1", "u2"]]);
    expect(within(screen.getByRole("table", { name: "Members" })).queryByText("kim")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Remove owner" }));
    fireEvent.click(within(screen.getByRole("group", { name: "Confirm" })).getByRole("button", { name: "Yes, remove owner" }));
    await flush();
    expect(screen.getByText(/last_owner/).closest(".error")?.textContent).toContain("last owner");
  });

  it("sets the project's default mode, with a sentence on what it means", async () => {
    const c = client();
    const changed: string[] = [];
    render(<ProjectSettings client={c} project={c.projectList[0]!} onClose={() => {}} onChanged={() => changed.push("changed")} />);
    await screen.findByRole("table", { name: "Members" });
    const mode = screen.getByLabelText("The project's default mode") as HTMLSelectElement;
    expect(mode.value).toBe("readonly");
    expect(screen.getByText(/above the default waits for/)).toBeTruthy();
    fireEvent.change(mode, { target: { value: "auto" } });
    fireEvent.click(screen.getByRole("button", { name: "Save the default mode" }));
    await flush();
    expect(c.args.setDefaultMode).toEqual([["p1", "auto"]]);
    expect(changed).toEqual(["changed"]);
    expect(screen.getByRole("status").textContent).toContain("auto");
  });

  it("shows a refused read", async () => {
    const c = client();
    c.refuse.members = new AdaError("local_only", 403, "only the owner's desktop can do this");
    open(c);
    expect(await screen.findByText(/local_only/)).toBeTruthy();
  });

  it("names every control and labels the dialog", async () => {
    const c = client();
    open(c);
    await screen.findByRole("table", { name: "Members" });
    fireEvent.click(screen.getByRole("button", { name: "Remove kim" }));
    const dialog = document.querySelector("dialog")!;
    expect(dialog.getAttribute("aria-labelledby")).toBeTruthy();
    expect(document.getElementById(dialog.getAttribute("aria-labelledby")!)?.textContent).toContain("repo");
    const unnamed = Array.from(dialog.querySelectorAll("button, input, select")).filter((el) => {
      const label = el.getAttribute("aria-label") ?? ((el as HTMLInputElement).labels?.[0]?.textContent ?? el.textContent ?? "");
      return label.trim() === "";
    });
    expect(unnamed.map((el) => el.outerHTML.slice(0, 80))).toEqual([]);
    // The way out holds focus, so Enter alone never confirms.
    expect(document.activeElement?.textContent).toBe("Cancel");
  });
});

describe("opening project settings", () => {
  it("opens from the project in the desktop's navigation, and the browser has no such control", async () => {
    const c = client();
    render(<App client={c} />);
    fireEvent.click(await screen.findByRole("button", { name: "Settings for repo" }));
    await waitFor(() => expect(screen.getByRole("table", { name: "Members" })).toBeTruthy());
    cleanup();
    const b = client();
    b.channel = "network";
    render(<App client={b} account={{ id: "local", name: "owner", owner: true, password_allowed: false }} />);
    await flush();
    expect(screen.queryByRole("button", { name: "Settings for repo" })).toBeNull();
  });
});
