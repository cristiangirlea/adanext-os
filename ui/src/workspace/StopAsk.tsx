import type { ErrorDetails, StopCounts } from "../client/api";
import { BusyButton } from "../motion/parts";

// The owner's stop asks first: what it would end, other users' work above all, and only then
// stops with the count of other users' attempts the owner saw. adad refuses a count that moved.

// The reason adad records on every attempt the stop ends.
export const stopReason = "coordinator stopped by the owner";

// The counts a stop_count_changed refusal carries; undefined when it carried none.
export function changedCounts(details: ErrorDetails | undefined): StopCounts | undefined {
  if (!details) return undefined;
  const { running, queued, others_running, others_queued } = details;
  if (typeof running !== "number" || typeof queued !== "number" || typeof others_running !== "number" || typeof others_queued !== "number") return undefined;
  return { running, queued, others_running, others_queued };
}

function others(counts: StopCounts): string {
  const running = counts.others_running;
  const queued = counts.others_queued;
  if (running === 0 && queued === 0) return "No task of other users is running or queued.";
  return `${running} ${running === 1 ? "task" : "tasks"} of other users ${running === 1 ? "is" : "are"} running and ${queued} ${queued === 1 ? "is" : "are"} queued. They will be cancelled with the reason “${stopReason}”.`;
}

export function StopAsk({
  counts,
  changed,
  quits,
  busy,
  onConfirm,
  onCancel,
}: {
  counts: StopCounts;
  // The owner confirmed once already and the counts moved since.
  changed: boolean;
  // The desktop closes once the work has stopped; the browser only stops it.
  quits: boolean;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ownRunning = Math.max(0, counts.running - counts.others_running);
  const ownQueued = Math.max(0, counts.queued - counts.others_queued);
  return (
    <div className="note confirm" role="group" aria-label="Stop work">
      {changed && <p>The counts changed since you looked; confirm again with these.</p>}
      <p>{others(counts)}</p>
      <p>
        Your own work: {ownRunning} running and {ownQueued} queued, stopped the same way. {quits ? "Ada then waits for the work to stop and closes." : "Ada stops once the work has; this page then loses its connection."}
      </p>
      <div className="actions">
        <BusyButton busy={busy} disabled={busy} className="danger" onClick={onConfirm}>
          {quits ? "Stop work and quit" : "Stop work"}
        </BusyButton>
        {/* Focus lands on the way out, so Enter alone never stops anything. */}
        <button type="button" className="quiet" autoFocus disabled={busy} onClick={onCancel}>
          Keep working
        </button>
      </div>
    </div>
  );
}
