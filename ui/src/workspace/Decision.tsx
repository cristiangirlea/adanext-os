import { useId } from "react";
import type { AttemptDecision } from "../client/api";
import { BusyButton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ErrorLine, type Shown } from "./errors";

// A pending attempt whose mode ranks above its project's default waits for a decision. adad
// sends the decision only to the attempt's actor (decision.actor) and the project's owners: the
// actor may allow or deny it, an owner who is not the actor may only deny it, anyone else sees
// nothing.

export function DecisionPanel({
  decision,
  canAllow,
  canDeny,
  busy,
  onDecide,
  lookupError = null,
  onRetry,
  identityKnown = true,
}: {
  decision: AttemptDecision;
  canAllow: boolean;
  canDeny: boolean;
  busy: "" | "allow" | "deny";
  onDecide: (choice: "allow" | "deny") => void;
  // The desktop could not read its own account, so it cannot tell whether it is the actor.
  lookupError?: Shown | null;
  onRetry?: () => void;
  // Whether the viewer's own id is known yet; until then the panel says nothing about who may allow.
  identityKnown?: boolean;
}) {
  const heading = useId();
  if (decision.allowed) return <p className="note">Allowed: the attempt starts when its turn in the queue comes.</p>;
  if (!decision.required || decision.denied) return null;
  return (
    <section className="note note-warn decision" aria-labelledby={heading}>
      <h3 id={heading}>Waiting for your decision</h3>
      <p>
        This attempt would run <Marked text={decision.scope.backend} /> in <strong>{decision.scope.mode}</strong> mode, above the project's default of <strong>{decision.project_default}</strong>. It keeps its place in the queue and does not start until it is allowed.
      </p>
      {lookupError && (
        <div className="error-block">
          <ErrorLine error={lookupError} alert={false} />
          <p>
            Ada could not tell whether you started this attempt, so Allow is held back.{" "}
            <button type="button" className="quiet" onClick={onRetry}>
              Try again
            </button>
          </p>
        </div>
      )}
      {identityKnown && canDeny && !canAllow && !lookupError && <p>Only the person who started it can allow it; as a project owner you can deny it.</p>}
      {(canAllow || canDeny) && (
        <div className="actions">
          {canAllow && (
            <BusyButton busy={busy === "allow"} disabled={busy !== ""} onClick={() => onDecide("allow")}>
              Allow
            </BusyButton>
          )}
          <BusyButton busy={busy === "deny"} disabled={busy !== ""} className="danger" onClick={() => onDecide("deny")}>
            Deny
          </BusyButton>
        </div>
      )}
    </section>
  );
}
