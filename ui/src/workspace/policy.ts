import type { Capabilities, CapabilityCell, ExecutionPolicy } from "../client/api";

// The execution policy offered from the capability matrix. Readonly is the default mode and yolo
// is never preselected (decisions log, 2026-10-20): defaultPolicy and every backend change land
// on readonly, and only the owner's own click picks another mode.

export const modeOrder = ["readonly", "auto", "yolo"] as const;

export const rowNames: Record<string, string> = {
  read_only: "Read-only",
  workspace_writes_and_shell: "Writes and shell",
  network: "Network",
  approvals: "Approvals",
  continuation: "Continuation",
  cancellation: "Cancellation",
  usage_reporting: "Usage reporting",
  model_discovery: "Model discovery",
  time_limit: "Time limit",
  turn_limit: "Turn limit",
};

// The caps a start may set, offered only where the backend's options list them.
export const timeLimits = ["2m", "5m", "10m", "30m"];
export const turnLimits = [3, 5, 10, 25];

// withLimit sets or clears one cap, so an unset cap is never sent.
export function withLimit(policy: ExecutionPolicy, key: "timeout" | "max_turns", value: string): ExecutionPolicy {
  const next = { ...policy };
  if (value === "") delete next[key];
  else if (key === "timeout") next.timeout = value;
  else next.max_turns = Number(value);
  return next;
}

// The rows a mode card shows; the rest are under "Every row".
export const cardRows = ["read_only", "workspace_writes_and_shell", "network"];

export function defaultPolicy(matrix: Capabilities | null): ExecutionPolicy {
  const backends = matrix?.backends ?? [];
  const installed = backends.find((b) => b.installed?.available) ?? backends[0];
  return { backend: installed?.backend ?? "claude", model: "", effort: "", mode: "readonly" };
}

// What a cell claims, in words.
export function claim(cell: CapabilityCell): string {
  switch (cell.status) {
    case "enforced":
      return cell.by ? `enforced by ${cell.by}` : "enforced";
    case "requested":
      return cell.by ? `asked of ${cell.by}, not verified` : "asked, not verified";
    case "unsupported":
      return "not offered";
    case "unverified":
      return `unverified for the installed CLI${cell.recorded ? ` (recorded ${cell.recorded})` : ""}`;
    case "unsandboxed":
      return "unsandboxed";
    default:
      return cell.status;
  }
}

// What yolo turns off, plan section 4: permission prompts and the read-only boundary; the agent
// runs any tool as the user on the host.
export const yoloDisables = [
  "every permission prompt: each tool the agent calls runs without asking",
  "the read-only boundary: the agent may change any file it can reach, not only the worktree",
  "any limit on commands: it runs them as you, on this computer, with your network",
];
