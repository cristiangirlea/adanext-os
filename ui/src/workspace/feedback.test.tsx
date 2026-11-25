import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { AttemptDecision, ProjectSummary } from "../client/api";
import { FakeClient } from "../test/fakeClient";
import { DecisionPanel } from "./Decision";
import { BlockerList } from "./ImportFlow";
import { SnapshotTask } from "./SnapshotTask";

// The snapshot form's project choice, a redacted blocker's count, and what the decision panel
// says while the desktop's own identity is still being read.

afterEach(cleanup);

const repo: ProjectSummary = { id: "p1", name: "repo", role: "owner", created: "", default_mode: "readonly" };

describe("the snapshot form", () => {
  it("snapshots the one project that arrived after mount, without the select changing", async () => {
    const client = new FakeClient();
    const props = { client, matrix: null, defaultProject: "", busy: false, onCreate: () => {} };
    const { rerender } = render(<SnapshotTask {...props} projects={[]} />);
    rerender(<SnapshotTask {...props} projects={[repo]} />);
    expect((screen.getByLabelText("Project") as HTMLSelectElement).value).toBe("p1");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Take a snapshot" }));
    });
    expect(client.args.snapshotProject).toEqual([["p1", { untracked: [], include_sensitive: [], include_changes: true }]]);
  });
});

describe("a blocker whose paths were redacted", () => {
  it("still shows how many paths it holds", () => {
    render(<BlockerList blockers={[{ code: "unmerged", paths: [], count: 3 }]} />);
    expect(screen.getByText("3 paths")).toBeTruthy();
  });
});

describe("the decision panel while the viewer's identity is read", () => {
  const decision: AttemptDecision = { actor: "u1", required: true, scope: { backend: "claude", mode: "auto", workspace: "w" }, project_default: "readonly", allowed: false, denied: false };
  it("says nothing about who may allow until the identity is known", () => {
    const { rerender } = render(<DecisionPanel decision={decision} canAllow={false} canDeny identityKnown={false} busy="" onDecide={() => {}} />);
    expect(screen.queryByText(/Only the person who started it/)).toBeNull();
    rerender(<DecisionPanel decision={decision} canAllow={false} canDeny identityKnown busy="" onDecide={() => {}} />);
    expect(screen.getByText(/Only the person who started it/)).toBeTruthy();
  });
});
