import { useState } from "react";
import type { AdaClient, Capabilities, CheckLog, CheckResult } from "../client/api";
import { Marked } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "./errors";
import { UnsandboxedNotice } from "./TaskForm";

// Check results (M4.2), with the baseline beside each so an old failure reads apart from a new
// one, and each log read on demand as text.

const stateNames: Record<string, string> = {
  running: "running",
  passed: "passed",
  failed: "failed",
  timed_out: "timed out",
  killed: "killed",
  not_run: "not run",
  interrupted: "interrupted",
  changed_tree: "changed tracked files",
};

function verdict(r: CheckResult): string {
  if (r.state === "passed") return "passed";
  if (r.baseline_failure) return "already failing before the task";
  if (r.new_failure) return "new failure";
  return stateNames[r.state] ?? r.state;
}

function CheckItem({ client, taskId, result }: { client: AdaClient; taskId: string; result: CheckResult }) {
  const [log, setLog] = useState<CheckLog | null>(null);
  const [error, setError] = useState<Shown | null>(null);
  const [loading, setLoading] = useState(false);
  const read = async () => {
    setLoading(true);
    setError(null);
    try {
      setLog(await client.checkLog(taskId, result.id));
    } catch (e) {
      setError(shown(e));
    } finally {
      setLoading(false);
    }
  };
  const name = result.name || result.definition.argv.join(" ");
  return (
    <article className="check" data-state={result.state} data-new-failure={result.new_failure || undefined}>
      <header>
        <h4>
          <Marked text={name} />
        </h4>
        <span className={`check-state check-${result.state}`}>{verdict(result)}</span>
      </header>
      <p className="check-argv">
        {result.definition.argv.map((word, i) => (
          <code key={i} className="argv-word">
            <Marked text={word} />
          </code>
        ))}
        {result.definition.workdir && (
          <>
            {" "}
            in <code>
              <Marked text={result.definition.workdir} />
            </code>
          </>
        )}
      </p>
      <p className="check-facts">
        {result.exit_code !== undefined && <span>exit {result.exit_code}</span>}
        <span>{(result.duration_ms / 1000).toFixed(1)} s</span>
        {result.tested_commit && (
          <span>
            at <code>{result.tested_commit.slice(0, 12)}</code>
          </span>
        )}
        {result.baseline && <span>baseline {stateNames[result.baseline.state] ?? result.baseline.state}</span>}
        {result.env_keys.length > 0 && (
          <span>
            environment <Marked text={result.env_keys.join(", ")} />
          </span>
        )}
      </p>
      {result.invalidated && <p className="note note-warn">The branch changed after this check ran; its result no longer binds the current commit.</p>}
      {result.state === "changed_tree" && <p className="note note-warn">This check changed tracked files; its result binds nothing.</p>}
      {log ? (
        <div className="log">
          <pre aria-label={`Log of ${name}`}>
            <Marked text={log.text} keepNewlines />
          </pre>
          {log.truncated && <p className="note">The log was cut at the check's output cap.</p>}
          {log.partial && <p className="note">Only the first 1 MiB is shown.</p>}
        </div>
      ) : (
        <button type="button" className="quiet" aria-busy={loading || undefined} onClick={() => void read()}>
          Show the log of {name}
        </button>
      )}
      {error && <ErrorLine error={error} alert={false} />}
    </article>
  );
}

export function ChecksView({ client, taskId, results, matrix }: { client: AdaClient; taskId: string; results: CheckResult[]; matrix: Capabilities | null }) {
  return (
    <div className="checks">
      <UnsandboxedNotice matrix={matrix} />
      {results.length === 0 ? <p className="empty">No check has run for this task.</p> : results.map((r) => <CheckItem key={r.id} client={client} taskId={taskId} result={r} />)}
    </div>
  );
}
