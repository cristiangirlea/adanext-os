import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import type { Capabilities, Changes, CheckResult, ExecutionPolicy } from "../client/api";
import { Marked, PathText, hiddenIn } from "../text/Marked";
import { capabilities, FakeClient } from "../test/fakeClient";
import { Activity, describeEvent } from "./Activity";
import { parseArgv } from "./argv";
import { ChangesView, Patch } from "./Changes";
import { ChecksView } from "./Checks";
import { FilesView } from "./Files";
import { defaultPolicy } from "./policy";
import { PolicyPicker } from "./PolicyPicker";
import { sensitive } from "./sensitive";

afterEach(cleanup);

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

function Picker({ initial, onChange, matrix = capabilities() }: { initial?: ExecutionPolicy; onChange?: (p: ExecutionPolicy) => void; matrix?: Capabilities }) {
  const [value, setValue] = useState(initial ?? defaultPolicy(matrix));
  return (
    <PolicyPicker
      matrix={matrix}
      value={value}
      onChange={(next) => {
        setValue(next);
        onChange?.(next);
      }}
    />
  );
}

describe("the policy picker", () => {
  it("preselects readonly and never yolo, for every backend of the matrix", () => {
    const matrix = capabilities();
    expect(defaultPolicy(matrix)).toEqual({ backend: "claude", model: "", effort: "", mode: "readonly" });
    expect(defaultPolicy(null).mode).toBe("readonly");
    render(<Picker />);
    expect((screen.getByRole("radio", { name: /readonly/ }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole("radio", { name: /yolo/ }) as HTMLInputElement).checked).toBe(false);
    for (const backend of matrix.backends.map((b) => b.backend)) {
      fireEvent.change(screen.getByLabelText("Backend"), { target: { value: backend } });
      expect((screen.getByRole("radio", { name: /readonly/ }) as HTMLInputElement).checked).toBe(true);
      expect((screen.getByRole("radio", { name: /yolo/ }) as HTMLInputElement).checked).toBe(false);
    }
  });

  it("goes back to readonly when the backend changes after yolo was chosen", () => {
    const seen: ExecutionPolicy[] = [];
    render(<Picker onChange={(p) => seen.push(p)} />);
    fireEvent.click(screen.getByRole("radio", { name: /yolo/ }));
    expect(seen.at(-1)?.mode).toBe("yolo");
    fireEvent.change(screen.getByLabelText("Backend"), { target: { value: "codex" } });
    expect(seen.at(-1)).toEqual({ backend: "codex", model: "", effort: "", mode: "readonly" });
  });

  it("offers a time and a turn limit only where the backend takes them, and sends none unset", () => {
    const matrix = capabilities();
    matrix.backends[0]!.options = ["model", "mode", "timeout", "max_turns"];
    matrix.backends[1]!.options = ["model", "mode", "timeout"];
    const seen: ExecutionPolicy[] = [];
    render(<Picker matrix={matrix} onChange={(p) => seen.push(p)} />);
    fireEvent.change(screen.getByLabelText("Time limit"), { target: { value: "5m" } });
    fireEvent.change(screen.getByLabelText("Turn limit"), { target: { value: "3" } });
    expect(seen.at(-1)).toEqual({ backend: "claude", model: "", effort: "", mode: "readonly", timeout: "5m", max_turns: 3 });
    fireEvent.change(screen.getByLabelText("Turn limit"), { target: { value: "" } });
    expect(seen.at(-1)).toEqual({ backend: "claude", model: "", effort: "", mode: "readonly", timeout: "5m" });
    fireEvent.change(screen.getByLabelText("Backend"), { target: { value: "codex" } });
    expect(screen.getByLabelText("Time limit")).toBeTruthy();
    expect(screen.queryByLabelText("Turn limit")).toBeNull();
    cleanup();
    render(<Picker />);
    expect(screen.queryByLabelText("Time limit")).toBeNull();
  });

  it("shows what yolo turns off, what each mode actually holds, and the refused ask mode", () => {
    render(<Picker />);
    const yolo = screen.getByRole("radio", { name: /yolo/ }).closest(".mode") as HTMLElement;
    expect(within(yolo).getByText("Turns off")).toBeTruthy();
    expect(within(yolo).getByText(/every permission prompt/)).toBeTruthy();
    const readonly = screen.getByRole("radio", { name: /readonly/ }).closest(".mode") as HTMLElement;
    expect(within(readonly).getByText("asked of claude --permission-mode plan, not verified")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Backend"), { target: { value: "codex" } });
    const codexReadonly = screen.getByRole("radio", { name: /readonly/ }).closest(".mode") as HTMLElement;
    expect(within(codexReadonly).getByText("enforced by codex --sandbox read-only")).toBeTruthy();
    expect(screen.getByText(/ask: mode ask needs approvals/)).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /^ask/ })).toBeNull();
  });

  it("offers the matrix's models and efforts, with the CLI's default first", () => {
    render(<Picker />);
    const models = Array.from((screen.getByLabelText("Model") as HTMLSelectElement).options).map((o) => o.value);
    expect(models).toEqual(["", "claude-opus-5-5"]);
    fireEvent.change(screen.getByLabelText("Backend"), { target: { value: "codex" } });
    expect(Array.from((screen.getByLabelText("Model") as HTMLSelectElement).options).map((o) => o.value)).toEqual(["", "gpt-6.1-sol", "gpt-6-luna"]);
  });
});

describe("text from a repository", () => {
  it("marks control and bidirectional characters in a path", () => {
    const path = "src/evil\u202Etxt.exe\u0007";
    render(<PathText path={path} />);
    const marks = Array.from(document.querySelectorAll("mark.hidden-char")).map((m) => m.textContent);
    expect(marks).toEqual(["U+202E", "U+0007"]);
    expect(screen.getByText("hidden characters")).toBeTruthy();
    expect(hiddenIn(path)).toEqual(["U+202E", "U+0007"]);
    expect(hiddenIn("plain/path.ts")).toEqual([]);
  });

  it("marks zero-width and isolate characters but keeps tabs, and newlines when asked", () => {
    const { container } = render(<Marked text={"a\u200Bb\u2066c\td\ne"} keepNewlines />);
    expect(Array.from(container.querySelectorAll("mark")).map((m) => m.textContent)).toEqual(["U+200B", "U+2066"]);
    expect(container.textContent).toContain("c\td\ne");
    cleanup();
    const inline = render(<Marked text={"a\nb"} />);
    expect(inline.container.querySelector("mark")?.textContent).toBe("U+000A");
  });

  it("shows a patch holding HTML as text, never as elements", () => {
    const patch = "diff --git a/x.html b/x.html\n--- a/x.html\n+++ b/x.html\n@@ -1 +1 @@\n-<p>old</p>\n+<img src=x onerror=alert(1)><script>alert(2)</script>\n";
    const { container } = render(<Patch text={patch} />);
    expect(container.querySelector("img, script, p")).toBeNull();
    const added = container.querySelector(".patch-add");
    expect(added?.textContent).toBe("+<img src=x onerror=alert(1)><script>alert(2)</script>");
    expect(container.querySelector(".patch-del")?.textContent).toBe("-<p>old</p>");
    expect(Array.from(container.querySelectorAll(".patch-meta")).map((l) => l.textContent)).toEqual(["diff --git a/x.html b/x.html", "--- a/x.html", "+++ b/x.html"]);
  });

  it("marks hidden characters inside patch lines", () => {
    const { container } = render(<Patch text={"@@ -1 +1 @@\n+if (isAdmin\u202E) {\n"} />);
    expect(container.querySelector(".patch-add mark")?.textContent).toBe("U+202E");
  });
});

function changeSet(fields: Partial<Changes> = {}): Changes {
  return {
    task: "t1",
    attempt: "a1",
    base: "b".repeat(40),
    commit: "c".repeat(40),
    tip: "c".repeat(40),
    stale: false,
    generation: 3,
    files: [],
    total: 0,
    files_truncated: false,
    bytes_truncated: false,
    total_capped: false,
    provider_changes: null,
    provider_flagged: false,
    checks: [],
    ...fields,
  };
}

describe("the changes view", () => {
  it("lists each file with its status, renames, binaries, provider marks and cuts", () => {
    render(
      <ChangesView
        changes={changeSet({
          total: 3,
          files_truncated: true,
          provider_flagged: true,
          provider_changes: { changes: [{ path: ".claude/settings.json", change: "added" }], truncated: false },
          files: [
            { path: "new.txt", old_path: "old.txt", status: "R", similarity: 90, old_mode: "100644", new_mode: "100755", binary: false, added: 1, deleted: 0, patch: "@@ -1 +1 @@\n+x\n", patch_truncated: true, provider: false },
            { path: "logo.png", status: "A", old_mode: "", new_mode: "100644", binary: true, added: 0, deleted: 0, patch: "", patch_truncated: false, provider: false },
            { path: ".claude/settings.json", status: "A", old_mode: "", new_mode: "100644", binary: false, added: 2, deleted: 0, patch: "@@ -0,0 +1,2 @@\n+{}\n", patch_truncated: false, provider: true },
          ],
        })}
      />,
    );
    expect(screen.getByText("renamed")).toBeTruthy();
    expect(screen.getByText("old.txt")).toBeTruthy();
    expect(screen.getByText("mode 100644 to 100755")).toBeTruthy();
    expect(screen.getByText("binary")).toBeTruthy();
    expect(screen.getByText("A binary file; no patch is shown.")).toBeTruthy();
    expect(screen.getByText("provider configuration")).toBeTruthy();
    expect(screen.getByText(/changed provider configuration or hooks/)).toBeTruthy();
    expect(screen.getByText("The patch was cut at 512 KiB.")).toBeTruthy();
    expect(screen.getByText(/Only the first 1000 files/)).toBeTruthy();
  });

  it("says when the branch moved past the changes shown", () => {
    render(<ChangesView changes={changeSet({ stale: true })} />);
    expect(screen.getByText(/branch has moved past this commit/)).toBeTruthy();
    expect(screen.getByText("The agent changed no files.")).toBeTruthy();
  });
});

function result(fields: Partial<CheckResult> & { id: string }): CheckResult {
  return { ord: 0, definition: { argv: ["go", "test", "./..."] }, state: "passed", invalidated: false, output_size: 0, truncated: false, new_failure: false, baseline_failure: false, env_keys: [], duration_ms: 1500, ...fields };
}

describe("the checks view", () => {
  it("says checks are unsandboxed and tells a new failure from one that existed before", async () => {
    const client = new FakeClient();
    client.logs.c2 = { text: "FAIL <b>x</b>\u202E\n", truncated: true, partial: false };
    render(
      <ChecksView
        client={client}
        taskId="t1"
        matrix={capabilities()}
        results={[
          result({ id: "c1", name: "unit", state: "failed", exit_code: 1, baseline_failure: true, baseline: { id: "b1", state: "failed", tested_commit: "b", output_size: 0, truncated: false, duration_ms: 1 } }),
          result({ id: "c2", name: "lint", state: "failed", exit_code: 2, new_failure: true, invalidated: true }),
        ]}
      />,
    );
    expect(screen.getByText("Checks are not sandboxed.")).toBeTruthy();
    expect(screen.getByText("already failing before the task")).toBeTruthy();
    expect(screen.getByText("new failure")).toBeTruthy();
    expect(screen.getByText(/no longer binds the current commit/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Show the log of lint" }));
    await flush();
    expect(client.args.checkLog).toEqual([["t1", "c2"]]);
    const log = screen.getByLabelText("Log of lint");
    expect(log.querySelector("b")).toBeNull();
    expect(log.textContent).toContain("FAIL <b>x</b>U+202E");
    expect(screen.getByText("The log was cut at the check's output cap.")).toBeTruthy();
  });
});

describe("the files view", () => {
  it("walks the commit's tree through the files route, shows a link's target without following it, and a file as text", async () => {
    const client = new FakeClient();
    const commit = "c".repeat(40);
    client.fileViews[""] = {
      task: "t1",
      commit,
      path: "",
      type: "tree",
      mode: "040000",
      size: 0,
      truncated: false,
      entries: [
        { name: "src", path: "src", type: "tree", mode: "040000", size: 0 },
        { name: "out", path: "out", type: "symlink", mode: "120000", size: 0 },
      ],
    };
    client.fileViews.src = { task: "t1", commit, path: "src", type: "tree", mode: "040000", size: 0, truncated: false, entries: [{ name: "a.html", path: "src/a.html", type: "blob", mode: "100644", size: 20 }] };
    client.fileViews["src/a.html"] = { task: "t1", commit, path: "src/a.html", type: "blob", mode: "100644", size: 20, truncated: false, encoding: "utf-8", content: "<script>x</script>\n" };
    client.fileViews.out = { task: "t1", commit, path: "out", type: "symlink", mode: "120000", size: 0, truncated: false, target: "../../etc" };
    render(<FilesView client={client} taskId="t1" commit={commit} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "out" }));
    await flush();
    expect(screen.getByText(/never follows it/)).toBeTruthy();
    expect(screen.getByText("../../etc")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "root" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "src/" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "a.html" }));
    await flush();
    const content = screen.getByLabelText("Content of src/a.html");
    expect(content.querySelector("script")).toBeNull();
    expect(content.textContent).toBe("<script>x</script>\n");
    expect(client.args.files!.map((a) => a[1])).toEqual(["", "out", "", "src", "src/a.html"]);
  });
});

describe("the activity", () => {
  it("renders events by kind, as text, instead of raw JSON", () => {
    const line = (event: Record<string, unknown>) => ({ v: 1, type: "event", attempt_id: "a1", event });
    const events = [
      { seq: 1, kind: "text", payload: line({ kind: "text", text: "I read <b>main.go</b>" }) },
      { seq: 2, kind: "tool_call", payload: line({ kind: "tool_call", tool: "Bash", text: "go test" }) },
      { seq: 3, kind: "result", payload: { v: 1, type: "result", result: { status: "ok" } } },
      { seq: 4, kind: "executor_error", payload: { error: "preparing deadline exceeded" } },
      { seq: 5, kind: "cancel", payload: { reason: "coordinator stopped by the owner" } },
    ];
    render(<Activity events={events} stream="connected" hasAttempt />);
    const items = within(screen.getByRole("list", { name: "Events" })).getAllByRole("listitem");
    expect(items.map((i) => i.getAttribute("data-seq"))).toEqual(["1", "2", "3", "4", "5"]);
    expect(items[0]!.textContent).toContain("I read <b>main.go</b>");
    expect(items[0]!.querySelector("b")).toBeNull();
    expect(items[0]!.textContent).not.toContain('"attempt_id"');
    expect(within(items[1]!).getByText("Tool Bash")).toBeTruthy();
    expect(within(items[2]!).getByText("ok")).toBeTruthy();
    expect(items[2]!.getAttribute("data-tone")).toBe("good");
    expect(within(items[3]!).getByText("preparing deadline exceeded")).toBeTruthy();
    expect(within(items[4]!).getByText("coordinator stopped by the owner")).toBeTruthy();
    expect(describeEvent({ seq: 9, kind: "something_new", payload: { a: 1 } }).text).toBe('{"a":1}');
  });
});

describe("check commands and credentials", () => {
  it("splits a command line into argv with quotes and no shell", () => {
    expect(parseArgv("go test ./...")).toEqual(["go", "test", "./..."]);
    expect(parseArgv(`npm run "unit tests" -- --grep 'a b'`)).toEqual(["npm", "run", "unit tests", "--", "--grep", "a b"]);
    expect(parseArgv("echo a | rm -rf $HOME")).toEqual(["echo", "a", "|", "rm", "-rf", "$HOME"]);
    expect(parseArgv("C:\\tools\\lint.exe x\\ y")).toEqual(["C:\\tools\\lint.exe", "x y"]);
    expect(parseArgv('say "unclosed')).toEqual({ error: "a double quote is not closed" });
    expect(parseArgv("  ")).toEqual([]);
  });

  it("marks the same paths adad treats as credentials", () => {
    for (const p of [".env", ".env.local", "keys/id_ed25519", "a/.ssh/config", "x/.aws/c", "cert.pem", "creds/credentials", ".npmrc"]) expect(sensitive(p), p).toBe(true);
    for (const p of ["notes.txt", "src/env.ts", "docs/key-ideas.md"]) expect(sensitive(p), p).toBe(false);
  });
});
