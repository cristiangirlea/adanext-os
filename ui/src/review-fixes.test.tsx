import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { AdaError, type ChangedFile, type Changes } from "./client/api";
import { FakeClient, makeTask } from "./test/fakeClient";
import { Marked, hiddenIn } from "./text/Marked";
import { Activity } from "./workspace/Activity";
import { ChangesView } from "./workspace/Changes";
import { ChecksView } from "./workspace/Checks";

// The first review of M4.4b (PR #123): one test per finding.

afterEach(cleanup);

async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

const commit = "c".repeat(40);

function flagged(id: string, objective: string) {
  return makeTask({
    id,
    objective,
    state: "awaiting_review",
    revision: 2,
    attempt: { id: `a-${id}`, state: "ended_ok", tested_commit: commit, provider_flagged: true, provider_changes: { changes: [{ path: `.claude/${id}.json`, change: "added" }], truncated: false } },
  });
}

describe("the review panel belongs to its task", () => {
  it("does not carry an acknowledgement of provider changes from one task to the next", async () => {
    const client = new FakeClient([flagged("t1", "first"), flagged("t2", "second")]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("first"));
    await flush();
    fireEvent.click(screen.getByText("Refine: a new attempt with the findings"));
    fireEvent.click(screen.getByLabelText(/I have read these changes/));
    expect((screen.getByRole("button", { name: "Refine" }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByText("second"));
    await flush();
    fireEvent.click(screen.getByText("Refine: a new attempt with the findings"));
    expect((screen.getByLabelText(/I have read these changes/) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("button", { name: "Refine" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("the status bar", () => {
  it("reads the agent from the attempt, also for one this window did not start", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "running", revision: 2, attempt: { id: "a1", state: "running", backend: "codex", model: "gpt-6.1-sol", effort: "low", mode: "auto" } })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    await flush();
    expect(within(screen.getByRole("contentinfo")).getByText("codex, gpt-6.1-sol, low effort, auto")).toBeTruthy();
  });
});

async function inspected(client: FakeClient) {
  render(<App client={client} />);
  await flush();
  fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
  const d = screen.getByRole("dialog", { name: "Import a repository" });
  fireEvent.click(within(d).getByRole("button", { name: "Choose a folder" }));
  await flush();
  return d;
}

describe("the changes offer", () => {
  it("is checked by default and can be declined, which the snapshot is told", async () => {
    const client = new FakeClient();
    const d = await inspected(client);
    const offer = within(d).getByRole("checkbox", { name: "Include these changes in the new feature branch" }) as HTMLInputElement;
    expect(offer.checked).toBe(true);
    fireEvent.click(offer);
    expect(within(d).getByText(/stay out of the new branch/)).toBeTruthy();
    fireEvent.click(within(d).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    expect(client.args.snapshot![0]![1]).toMatchObject({ include_changes: false });
  });

  it("sends include_changes true when the offer is kept", async () => {
    const client = new FakeClient();
    const d = await inspected(client);
    fireEvent.click(within(d).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    expect(client.args.snapshot![0]![1]).toMatchObject({ include_changes: true });
  });

  it("preselects no untracked file when there are more than a snapshot takes, and says so", async () => {
    const client = new FakeClient();
    client.inspection = { ...client.inspection, status: { ...client.inspection.status, untracked: Array.from({ length: 1001 }, (_, i) => `gen/f${i}.txt`) } };
    const d = await inspected(client);
    expect(within(d).getAllByRole("checkbox").filter((b) => (b as HTMLInputElement).checked && b.closest(".untracked"))).toHaveLength(0);
    expect(within(d).getByText(/at most 1000/)).toBeTruthy();
  });
});

describe("refusals carry their details to the page", () => {
  it("shows the blockers of an import refused at snapshot time, with their paths", async () => {
    const client = new FakeClient();
    client.refuse.snapshot = new AdaError("import_blocked", 409, "the repository cannot be captured as it stands", undefined, { blockers: [{ code: "merge_in_progress", paths: ["a‮b.txt"], count: 1 }] });
    const d = await inspected(client);
    fireEvent.click(within(d).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    expect(within(d).getByText("A merge is in progress")).toBeTruthy();
    expect(within(d).getByText("U+202E")).toBeTruthy();
  });

  it("shows the path a snapshot refused, marked", async () => {
    const client = new FakeClient();
    client.refuse.snapshot = new AdaError("sensitive_path", 422, "looks like a credential; list it in include_sensitive to take it", undefined, { path: "x‮.env" });
    const d = await inspected(client);
    fireEvent.click(within(d).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    const error = within(d).getByText(/looks like a credential; list it/).closest(".error-block") as HTMLElement;
    expect(within(error).getByText("U+202E")).toBeTruthy();
  });
});

describe("repository text is marked everywhere it shows", () => {
  it("marks the task's provider paths and says how many more there are", async () => {
    const client = new FakeClient([makeTask({ id: "t1", provider_configuration: { paths: [".claude/a‮b"], count: 4 } })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    await flush();
    const note = screen.getByText(/The worktree holds provider configuration/).closest(".note") as HTMLElement;
    expect(within(note).getByText("U+202E")).toBeTruthy();
    expect(within(note).getByText(/and 3 more/)).toBeTruthy();
  });

  it("says how many more provider paths an inspection found", async () => {
    const client = new FakeClient();
    client.inspection = { ...client.inspection, findings: [{ code: "provider_configuration", paths: [".mcp.json"], count: 5 }] };
    const d = await inspected(client);
    expect(within(within(d).getByRole("group", { name: "Provider configuration" })).getByText("and 4 more")).toBeTruthy();
  });

  it("marks a check's working directory and environment keys", () => {
    render(
      <ChecksView
        client={new FakeClient()}
        taskId="t1"
        matrix={null}
        results={[{ id: "c1", ord: 0, name: "unit", definition: { argv: ["go"], workdir: "sub‮dir" }, state: "passed", invalidated: false, output_size: 0, truncated: false, new_failure: false, baseline_failure: false, env_keys: ["GO​FLAGS"], duration_ms: 1 }]}
      />,
    );
    const marks = Array.from(document.querySelectorAll("mark.hidden-char")).map((m) => m.textContent);
    expect(marks).toEqual(["U+202E", "U+200B"]);
  });

  it("marks a tool's name in the activity", () => {
    render(<Activity hasAttempt stream="" events={[{ seq: 1, kind: "tool_call", payload: { event: { kind: "tool_call", tool: "Ba‮sh", text: "x" } } }]} />);
    expect(document.querySelector(".event-label mark")?.textContent).toBe("U+202E");
  });
});

describe("hidden characters", () => {
  const classes: [string, string][] = [
    ["line separator", " "],
    ["paragraph separator", " "],
    ["tag character", "\u{E0041}"],
    ["soft hyphen", "­"],
    ["Mongolian vowel separator", "᠎"],
    ["invisible times", "⁢"],
    ["deprecated format character", "⁪"],
    ["interlinear annotation", "￹"],
    ["Hangul filler", "ㅤ"],
    ["Hangul choseong filler", "ᅟ"],
    ["halfwidth Hangul filler", "ﾠ"],
  ];
  for (const [name, ch] of classes) {
    it(`marks a ${name}`, () => {
      const { container } = render(<Marked text={`a${ch}b`} />);
      expect(container.querySelectorAll("mark")).toHaveLength(1);
      expect(hiddenIn(`a${ch}b`)).toHaveLength(1);
    });
  }
  it("still keeps tab, and newline when asked, and leaves plain letters alone", () => {
    expect(hiddenIn("a\tb")).toEqual([]);
    expect(hiddenIn("a\nb", true)).toEqual([]);
    expect(hiddenIn("café ß ü 日本")).toEqual([]);
  });
});

describe("patches are bounded in the page", () => {
  it("renders a file's patch only once the file is opened", () => {
    const file = (n: number): ChangedFile => ({ path: `f${n}.txt`, status: "M", old_mode: "100644", new_mode: "100644", binary: false, added: 1, deleted: 0, patch: "@@ -1 +1 @@\n+x\n", patch_truncated: false, provider: false });
    const changes: Changes = { task: "t1", attempt: "a1", base: "b".repeat(40), commit, tip: commit, stale: false, generation: 1, files: Array.from({ length: 30 }, (_, i) => file(i)), total: 30, files_truncated: false, bytes_truncated: false, total_capped: false, provider_changes: null, provider_flagged: false, checks: [] };
    const { container } = render(<ChangesView changes={changes} />);
    expect(container.querySelectorAll(".patch-line")).toHaveLength(0);
    const first = container.querySelector("details.file-change") as HTMLDetailsElement;
    first.open = true;
    fireEvent(first, new Event("toggle"));
    expect(container.querySelectorAll(".patch-line")).toHaveLength(2);
  });
});
