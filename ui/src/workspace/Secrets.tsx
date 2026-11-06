import type { SecretAck, SecretFinding, SecretScan } from "../client/api";
import { PathText } from "../text/Marked";

// The secret scan of the commit under review (#130): what the agent added that looks like a
// credential, never its value. Each open finding is answered before accept: a test value goes
// on the project's allow-list with its reason; a real secret is rotated by the user, and export
// refuses the branch while it holds the secret.

const ruleText: Record<string, string> = {
  "access-key-id": "a cloud access key id",
  "git-host-token": "a git hosting token",
  "private-key": "a private key",
  jwt: "a signed token",
  "chat-token": "a chat service token",
  "api-key": "an API key",
  "browser-api-key": "a browser API key",
  "live-payment-key": "a live payment key",
  "secret-assignment": "a secret-looking name set to a literal",
  "env-secret": "a secret-looking environment variable",
  "key-file": "a file that holds keys by its name",
};

export type SecretAnswers = Record<string, { kind: "test" | "real" | ""; reason: string }>;

// acksFor turns the answers into what accept sends, or null while an open finding is unanswered.
export function acksFor(scan: SecretScan | null, answers: SecretAnswers): SecretAck[] | null {
  if (!scan) return [];
  if (scan.findings_capped) return null;
  const acks: SecretAck[] = [];
  for (const f of scan.findings) {
    const a = answers[f.id];
    if (!a || a.kind === "") return null;
    if (a.kind === "test") {
      if (!a.reason.trim()) return null;
      acks.push({ finding: f.id, kind: "test", reason: a.reason.trim() });
    } else acks.push({ finding: f.id, kind: "real" });
  }
  return acks;
}

function where(f: SecretFinding) {
  return f.line > 0 ? `${f.path}:${f.line}` : f.path;
}

export function SecretFindings({ scan, answers, onAnswer }: { scan: SecretScan | null; answers: SecretAnswers; onAnswer: (id: string, answer: SecretAnswers[string]) => void }) {
  if (!scan) return <p className="hint">These changes were not scanned for secrets.</p>;
  if (scan.failed) return <p className="note note-warn">The secret scan could not read these changes: nothing in them was checked.</p>;
  const skipped = scan.not_scanned.length + scan.unlisted + (scan.capped ? 1 : 0);
  return (
    <div className="secrets" role="group" aria-label="Possible secrets">
      {scan.findings.length === 0 && skipped === 0 && !scan.capped && <p className="hint">No possible secrets in the lines these changes add.</p>}
      {scan.findings_capped && <p className="note note-warn">The changes hold more possible secrets than Ada records. Refine to remove them; this commit cannot be accepted.</p>}
      {scan.findings.length > 0 && (
        <div className="note note-warn">
          <p>These lines look like credentials. Answer each one to accept. Ada keeps where and which rule, never the value.</p>
          <ol className="secret-findings">
            {scan.findings.map((f) => {
              const a = answers[f.id] ?? { kind: "", reason: "" };
              return (
                <li key={f.id}>
                  <code>
                    <PathText path={f.path} raw={f.path_raw} />
                    {f.line > 0 && <span className="secret-line">:{f.line}</span>}
                  </code>{" "}
                  {ruleText[f.rule] ?? f.rule}
                  <fieldset className="secret-answer">
                    <legend className="visually-hidden">What is {where(f)}?</legend>
                    <label className="inline">
                      <input type="radio" name={`secret-${f.id}`} checked={a.kind === "test"} onChange={() => onAnswer(f.id, { ...a, kind: "test" })} /> A test value
                    </label>
                    <label className="inline">
                      <input type="radio" name={`secret-${f.id}`} checked={a.kind === "real"} onChange={() => onAnswer(f.id, { ...a, kind: "real" })} /> A real secret
                    </label>
                  </fieldset>
                  {a.kind === "test" && (
                    <label>
                      Why it is a test value (kept on the project's allow-list)
                      <input type="text" maxLength={1024} value={a.reason} onChange={(e) => onAnswer(f.id, { ...a, reason: e.target.value })} />
                    </label>
                  )}
                  {a.kind === "real" && (
                    <p className="hint">
                      Rotate it with its provider: the agent's CLI and its provider have seen it. Ada never tries a key. Export refuses this branch while it holds the
                      secret.
                    </p>
                  )}
                </li>
              );
            })}
          </ol>
        </div>
      )}
      {skipped > 0 && (
        <p className="hint">
          Not scanned for secrets:{" "}
          {scan.not_scanned.map((n, i) => (
            <span key={`${n.path}-${i}`}>
              {i > 0 && ", "}
              <code>
                <PathText path={n.path} raw={n.path_raw} />
              </code>{" "}
              ({n.reason === "binary" ? "binary" : "too large"})
            </span>
          ))}
          {(scan.unlisted > 0 || scan.capped) && `${scan.not_scanned.length ? ", and " : ""}${scan.unlisted || "some"}${scan.capped ? " or more" : ""} files past the listing cap`}.
        </p>
      )}
    </div>
  );
}
