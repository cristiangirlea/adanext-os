import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { AdaClient, OwnCredential, OwnSession, SignedInAccount } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { BusyButton, Skeleton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "../workspace/errors";

// The signed-in person's own account in the browser: where they are signed in, with this browser
// marked, and the passkeys and password they sign in with. Ending this browser's session, or
// changing a credential (which ends every session of the account), signs this page out.

function When({ iso }: { iso: string }) {
  const date = new Date(iso);
  return <time dateTime={iso}>{iso === "" || Number.isNaN(date.getTime()) ? "never" : date.toLocaleString()}</time>;
}

export function AccountPage({ client, account, onClose, onSignedOut }: { client: AdaClient; account: SignedInAccount; onClose: () => void; onSignedOut: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [sessions, setSessions] = useState<OwnSession[] | null>(null);
  const [credentials, setCredentials] = useState<OwnCredential[] | null>(null);
  const [error, setError] = useState<Shown | null>(null);
  const [busy, setBusy] = useState("");

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

  const load = useCallback(async () => {
    const [s, c] = await Promise.all([client.ownSessions(), client.ownCredentials()]);
    setSessions(s);
    setCredentials(c);
  }, [client]);
  useEffect(() => {
    // A 401 reloads the page (browser/Gate.tsx); anything else is shown here.
    load().catch((e: unknown) => setError(shown(e)));
  }, [load]);

  const act = async (what: string, fn: () => Promise<boolean>) => {
    setBusy(what);
    setError(null);
    try {
      if (await fn()) {
        onSignedOut();
        return;
      }
      await load();
    } catch (e) {
      setError(shown(e));
    }
    setBusy("");
  };
  // A credential goes only while another one the account can sign in with would remain.
  const usable = (c: OwnCredential) => c.kind === "passkey" || (c.kind === "password" && account.password_allowed);
  const removable = (c: OwnCredential) => (credentials ?? []).some((other) => other.id !== c.id && usable(other));
  const revoke = (s: OwnSession) =>
    void act(`session:${s.id}`, async () => {
      await client.revokeOwnSession(s.id);
      return s.current;
    });
  const remove = (c: OwnCredential) =>
    void act(`credential:${c.id}`, async () => {
      await client.removeOwnCredential(c.id);
      return true;
    });

  return (
    <dialog
      ref={dialog}
      className="import accounts"
      aria-labelledby="account-title"
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="import-head">
        <h2 id="account-title">
          Your account: <Marked text={account.name} />
        </h2>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div className="import-body">
        {error && <ErrorLine error={error} alert={false} />}
        <section aria-labelledby="account-sessions">
          <h3 id="account-sessions">Where you are signed in</h3>
          {sessions === null ? (
            error ? null : <Skeleton rows={2} label="Loading sessions" />
          ) : (
            <table className="tree accounts-table">
              <thead>
                <tr>
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
                      <Marked text={s.address} />
                      {s.current && <span className="tag">this browser</span>}
                    </td>
                    <td>
                      <When iso={s.created} />
                    </td>
                    <td>
                      <When iso={s.last_used} />
                    </td>
                    <td>
                      <BusyButton busy={busy === `session:${s.id}`} className="quiet" disabled={busy !== ""} aria-label={s.current ? "Sign out this browser" : `End the session from ${s.address}`} onClick={() => revoke(s)}>
                        {s.current ? "Sign out" : "End"}
                      </BusyButton>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
        <section aria-labelledby="account-credentials">
          <h3 id="account-credentials">How you sign in</h3>
          {credentials === null ? null : (
            <table className="tree accounts-table">
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Added</th>
                  <th scope="col">Last used</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {credentials.map((c) => (
                  <tr key={c.id} data-credential={c.id}>
                    <td>
                      <Marked text={c.name} />
                    </td>
                    <td>{c.kind}</td>
                    <td>
                      <When iso={c.created} />
                    </td>
                    <td>
                      <When iso={c.last_used} />
                    </td>
                    <td>
                      <BusyButton busy={busy === `credential:${c.id}`} className="quiet" disabled={busy !== "" || !removable(c)} aria-label={`Remove ${c.name}`} onClick={() => remove(c)}>
                        Remove
                      </BusyButton>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="hint">Removing a passkey or the password signs you out everywhere; sign in again with what is left. The last one cannot be removed.</p>
        </section>
      </div>
    </dialog>
  );
}
