import { useCallback, useEffect, useLayoutEffect, useRef, useState, type FormEvent } from "react";
import type { Account, AccountChange, AccountSession, AdaClient, EnrollmentCode } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { BusyButton, Skeleton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "../workspace/errors";

// The owner's accounts and the sessions signed in through the web. Only the desktop reaches
// these routes: adad serves them on its local channel to the owner and nowhere else.

// A code as the owner reads it out: 20 characters in four groups of five.
export function groupCode(code: string): string {
  const plain = code.replace(/[\s-]/g, "").toUpperCase();
  return plain.length === 20 ? plain.match(/.{5}/g)!.join("-") : code;
}

function When({ iso }: { iso: string }) {
  const date = new Date(iso);
  return <time dateTime={iso}>{Number.isNaN(date.getTime()) ? iso : date.toLocaleString()}</time>;
}

type Issued = EnrollmentCode & { name: string; kind: "created" | "issued" | "recovered" };
type Confirm = { kind: "remove"; account: Account } | { kind: "recover"; account: Account } | { kind: "revoke"; session: AccountSession; name: string };

function CodePanel({ issued, onDone }: { issued: Issued; onDone: () => void }) {
  const [copied, setCopied] = useState<"" | "copied" | "failed">("");
  const display = groupCode(issued.code);
  const copy = () => {
    // The page may lack clipboard access; the code stays selectable either way.
    const write = navigator.clipboard?.writeText(display);
    if (!write) {
      setCopied("failed");
      return;
    }
    write.then(
      () => setCopied("copied"),
      () => setCopied("failed"),
    );
  };
  return (
    <div className="note enroll" role="group" aria-label="Enrollment code">
      {issued.kind === "recovered" && (
        <p>
          <Marked text={issued.name} />: every session ended and the sign-in throttling cleared.
        </p>
      )}
      <p>
        Enrollment code for <Marked text={issued.name} />, shown only this once:
      </p>
      <p>
        <code className="enroll-code">{display}</code>{" "}
        <button type="button" className="quiet" onClick={copy}>
          Copy the code
        </button>
      </p>
      <p role="status">{copied === "copied" ? "Copied." : copied === "failed" ? "Could not copy: select the code and copy it." : ""}</p>
      <p>
        It works once, until <When iso={issued.expires} />. Once web access is on, the person enters it on their own computer at Ada's web address to add a passkey, or a password where one is allowed.
      </p>
      <button type="button" onClick={onDone}>
        Done
      </button>
    </div>
  );
}

function ConfirmPanel({ confirm, busy, onConfirm, onCancel }: { confirm: Confirm; busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  const text =
    confirm.kind === "remove" ? (
      <>
        Remove <Marked text={confirm.account.name} />? Its credentials, sessions and unused codes go with it, and this cannot be undone. An account with history in Ada cannot be removed: disable it instead.
      </>
    ) : confirm.kind === "recover" ? (
      <>
        Recover <Marked text={confirm.account.name} />? This ends every session of the account, clears its sign-in throttling and gives a new enrollment code, which replaces any unused one. Its passkeys and password stay.
      </>
    ) : (
      <>
        End the session of <Marked text={confirm.name} /> from <Marked text={confirm.session.address} />? That browser is signed out at once.
      </>
    );
  const label = confirm.kind === "remove" ? `Yes, remove ${confirm.account.name}` : confirm.kind === "recover" ? `Yes, recover ${confirm.account.name}` : "Yes, end the session";
  return (
    <div className="note confirm" role="group" aria-label="Confirm">
      <p>{text}</p>
      <div className="actions">
        <BusyButton busy={busy} className="danger" onClick={onConfirm}>
          {label}
        </BusyButton>
        {/* Focus lands on the way out, so Enter alone never confirms. */}
        <button type="button" className="quiet" autoFocus onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

export function Accounts({ client, onClose }: { client: AdaClient; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [sessions, setSessions] = useState<AccountSession[] | null>(null);
  const [error, setError] = useState<Shown | null>(null);
  const [busy, setBusy] = useState("");
  const [issued, setIssued] = useState<Issued | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [name, setName] = useState("");
  const [allowPassword, setAllowPassword] = useState(false);

  useLayoutEffect(() => {
    const el = dialog.current;
    if (!el) return;
    if (typeof el.showModal === "function") {
      if (!el.open) el.showModal();
    } else el.setAttribute("open", "");
    return () => {
      if (typeof el.close === "function" && el.open) el.close();
    };
  }, []);

  // Only the latest read is shown: an earlier one that answers later is stale.
  const reads = useRef(0);
  const load = useCallback(async () => {
    const read = ++reads.current;
    try {
      const [listed, signedIn] = await Promise.all([client.listAccounts(), client.listSessions()]);
      if (read !== reads.current) return;
      setAccounts(listed);
      setSessions(signedIn);
    } catch (e) {
      if (read === reads.current) throw e;
    }
  }, [client]);

  const act = async (what: string, fn: () => Promise<void>) => {
    setBusy(what);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(shown(e));
    }
    // Every change moves counts or sessions: read both back, refused or not, still busy.
    try {
      await load();
    } catch (e) {
      setError((current) => current ?? shown(e));
    } finally {
      setBusy("");
    }
  };

  useEffect(() => {
    load().catch((e: unknown) => setError(shown(e)));
  }, [load]);

  const create = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (trimmed === "") return;
    void act("create", async () => {
      const made = await client.createAccount({ name: trimmed, password_allowed: allowPassword });
      setIssued({ code: made.code, expires: made.expires, name: made.account.name, kind: "created" });
      setName("");
      setAllowPassword(false);
    });
  };
  const update = (account: Account, change: AccountChange, what: string) =>
    void act(`${what}:${account.id}`, async () => {
      await client.updateAccount(account.id, change);
    });
  const issue = (account: Account) =>
    void act(`code:${account.id}`, async () => {
      const code = await client.issueEnrollmentCode(account.id);
      setIssued({ ...code, name: account.name, kind: "issued" });
    });
  const confirmed = (c: Confirm) =>
    void act("confirm", async () => {
      try {
        if (c.kind === "remove") await client.removeAccount(c.account.id);
        else if (c.kind === "revoke") await client.revokeSession(c.session.id);
        else {
          const code = await client.recoverAccount(c.account.id);
          setIssued({ ...code, name: c.account.name, kind: "recovered" });
        }
      } finally {
        setConfirm(null);
      }
    });

  const nameOf = (id: string) => accounts?.find((a) => a.id === id)?.name ?? id;
  const idle = busy === "" && confirm === null;

  return (
    <dialog
      ref={dialog}
      className="import accounts"
      aria-labelledby="accounts-title"
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="import-head">
        <h2 id="accounts-title">Accounts</h2>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div className="import-body">
        {error && <ErrorLine error={error} alert={false} />}
        {issued && <CodePanel key={issued.code} issued={issued} onDone={() => setIssued(null)} />}
        {confirm && <ConfirmPanel confirm={confirm} busy={busy === "confirm"} onConfirm={() => confirmed(confirm)} onCancel={() => setConfirm(null)} />}

        <section aria-labelledby="accounts-people">
          <h3 id="accounts-people">People</h3>
          {accounts === null ? (
            error ? null : <Skeleton rows={2} label="Loading accounts" />
          ) : (
            <table className="tree accounts-table">
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Credentials</th>
                  <th scope="col">Sessions</th>
                  <th scope="col">Password</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => (
                  <tr key={a.id} data-account={a.id}>
                    <td>
                      <Marked text={a.name} />
                      {a.owner && <span className="tag">owner</span>}
                      {a.disabled && <span className="tag tag-warn">disabled</span>}
                    </td>
                    <td>{a.credentials}</td>
                    <td>{a.sessions}</td>
                    <td>
                      <input type="checkbox" aria-label={`Allow a password for ${a.name}`} checked={a.password_allowed} disabled={!idle} onChange={() => update(a, { password_allowed: !a.password_allowed }, "password")} />
                    </td>
                    <td className="row-actions">
                      <BusyButton busy={busy === `code:${a.id}`} className="quiet" disabled={!idle} aria-label={`New code for ${a.name}`} onClick={() => issue(a)}>
                        New code
                      </BusyButton>
                      <button type="button" className="quiet" disabled={!idle} aria-label={`Recover ${a.name}`} onClick={() => setConfirm({ kind: "recover", account: a })}>
                        Recover
                      </button>
                      {!a.owner && (
                        <>
                          <BusyButton busy={busy === `disable:${a.id}`} className="quiet" disabled={!idle} aria-label={`${a.disabled ? "Enable" : "Disable"} ${a.name}`} onClick={() => update(a, { disabled: !a.disabled }, "disable")}>
                            {a.disabled ? "Enable" : "Disable"}
                          </BusyButton>
                          <button type="button" className="quiet" disabled={!idle} aria-label={`Remove ${a.name}`} onClick={() => setConfirm({ kind: "remove", account: a })}>
                            Remove
                          </button>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="hint">
            The owner account cannot be disabled or removed. Disabling an account ends its sessions and stops it signing in; it keeps its history. A passkey is always allowed; a password only where ticked.
          </p>
        </section>

        <form className="task-form" onSubmit={create} aria-labelledby="accounts-new">
          <h3 id="accounts-new">New account</h3>
          <label>
            Name
            <input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="inline">
            <input type="checkbox" checked={allowPassword} onChange={(e) => setAllowPassword(e.target.checked)} />
            Allow a password as well as a passkey
          </label>
          <div>
            <BusyButton type="submit" busy={busy === "create"} disabled={!idle || name.trim() === ""}>
              Create the account
            </BusyButton>
          </div>
        </form>

        <section aria-labelledby="accounts-sessions">
          <h3 id="accounts-sessions">Sessions</h3>
          {sessions === null ? null : sessions.length === 0 ? (
            <p className="empty">No one is signed in through the web.</p>
          ) : (
            <table className="tree accounts-table">
              <thead>
                <tr>
                  <th scope="col">Account</th>
                  <th scope="col">Address</th>
                  <th scope="col">Signed in</th>
                  <th scope="col">Last used</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {sessions.map((s) => (
                  <tr key={s.id} data-session={s.id}>
                    <td>
                      <Marked text={nameOf(s.account)} />
                    </td>
                    <td>
                      <Marked text={s.address} />
                    </td>
                    <td>
                      <When iso={s.created} />
                    </td>
                    <td>
                      <When iso={s.last_used} />
                    </td>
                    <td>
                      <button type="button" className="quiet" disabled={!idle} aria-label={`Revoke the session of ${nameOf(s.account)} from ${s.address}`} onClick={() => setConfirm({ kind: "revoke", session: s, name: nameOf(s.account) })}>
                        Revoke
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      </div>
    </dialog>
  );
}
