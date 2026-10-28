import { useEffect, useState, type FormEvent } from "react";
import { AdaError, type AdaClient, type Capabilities, type Changes, type ExecutionPolicy, type Finding, type ProviderChanges, type Task } from "../client/api";
import { BusyButton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ProviderChangeList } from "./Changes";
import { ErrorLine, shown, type Shown } from "./errors";
import { defaultPolicy } from "./policy";
import { PolicyPicker } from "./PolicyPicker";

// The review of M4.3: findings any member can note, accept of the changes read in the work area
// (their commit and the coordinator's generation, so a later change refuses it), and refine, a
// new attempt with the findings in its prompt. Provider configuration an attempt changed is shown
// before a refine starts and must be acknowledged.

const refinable = new Set(["awaiting_review", "failed", "canceled"]);

export function Review({
  client,
  task,
  matrix,
  reviewed,
  onTask,
  onStarted,
}: {
  client: AdaClient;
  task: Task;
  matrix: Capabilities | null;
  reviewed: Changes | null;
  onTask: (task: Task) => void;
  onStarted: (task: Task, attemptId: string, policy: ExecutionPolicy) => void;
}) {
  const [findings, setFindings] = useState<Finding[]>([]);
  const [note, setNote] = useState("");
  const [error, setError] = useState<Shown | null>(null);
  const [busy, setBusy] = useState("");
  const [policy, setPolicy] = useState<ExecutionPolicy | null>(null);
  const [providerChanges, setProviderChanges] = useState<ProviderChanges | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const flagged = task.attempt?.provider_flagged === true;
  const shownChanges = providerChanges ?? (flagged ? (task.attempt?.provider_changes ?? null) : null);

  useEffect(() => {
    let live = true;
    client.findings(task.id).then(
      (list) => live && setFindings(list),
      (e: unknown) => live && setError(shown(e)),
    );
    return () => {
      live = false;
    };
  }, [client, task.id, task.attempt?.id]);

  const act = async (name: string, fn: () => Promise<void>) => {
    setBusy(name);
    setError(null);
    try {
      await fn();
    } catch (e) {
      if (e instanceof AdaError && e.code === "provider_configuration_changed" && e.details?.provider_changes) setProviderChanges(e.details.provider_changes);
      setError(shown(e));
    } finally {
      setBusy("");
    }
  };

  const addNote = (e: FormEvent) => {
    e.preventDefault();
    if (!note.trim()) return;
    void act("finding", async () => {
      const finding = await client.addFinding(task.id, note);
      setFindings((current) => [...current, finding]);
      setNote("");
    });
  };

  const current = reviewed && reviewed.task === task.id && !reviewed.stale ? reviewed : null;
  const chosen = policy ?? defaultPolicy(matrix);

  return (
    <section className="review" aria-labelledby="review-title">
      <h3 id="review-title">Review</h3>
      {error && <ErrorLine error={error} alert={false} />}
      {findings.length > 0 && (
        <ol className="findings" aria-label="Findings">
          {findings.map((f) => (
            <li key={f.id}>
              <span className="finding-author">{f.author}</span>
              <span className="finding-body">
                <Marked text={f.body} keepNewlines />
              </span>
            </li>
          ))}
        </ol>
      )}
      <form className="finding-form" onSubmit={addNote}>
        <label>
          Finding
          <textarea rows={2} value={note} onChange={(e) => setNote(e.target.value)} />
        </label>
        <BusyButton type="submit" className="quiet" busy={busy === "finding"} disabled={!note.trim()}>
          Add the finding
        </BusyButton>
      </form>
      {task.state === "awaiting_review" && (
        <div className="accept">
          <BusyButton
            busy={busy === "accept"}
            disabled={!current}
            onClick={() =>
              void act("accept", async () => {
                const accepted = await client.accept(task.id, task.revision, current!.commit, current!.generation);
                onTask(accepted.task);
              })
            }
          >
            Accept
          </BusyButton>
          <span className="hint">
            {current ? (
              <>
                Accepts commit <code>{current.commit.slice(0, 12)}</code> with its check results.
              </>
            ) : (
              "Read the current changes in the work area first."
            )}
          </span>
        </div>
      )}
      {task.state === "accepted" && <p className="accepted-note">Accepted. An accepted task is not refined.</p>}
      {refinable.has(task.state) && (
        <details className="refine">
          <summary>Refine: a new attempt with the findings</summary>
          {shownChanges && (
            <div className="note note-warn">
              <p>The last attempt changed provider configuration or hooks, which the CLI loads from the worktree:</p>
              <ProviderChangeList changes={shownChanges} />
              <label className="inline">
                <input type="checkbox" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} /> I have read these changes; run the next attempt with them
              </label>
            </div>
          )}
          <PolicyPicker matrix={matrix} value={chosen} onChange={setPolicy} legend="Refine with" />
          <BusyButton
            busy={busy === "refine"}
            disabled={shownChanges !== null && !acknowledged}
            onClick={() =>
              void act("refine", async () => {
                const refined = await client.refine(task.id, task.revision, { ...chosen, acknowledge_provider_changes: acknowledged });
                const { attempt_id, findings: _carried, ...next } = refined; // the ids of the findings its prompt carries
                onStarted(next, attempt_id, chosen);
              })
            }
          >
            Refine
          </BusyButton>
        </details>
      )}
    </section>
  );
}
