import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from "react";
import type { AdaClient, ListenerStatus } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { BusyButton, Skeleton } from "../motion/parts";
import { ErrorLine, shown, type Shown } from "../workspace/errors";

// The owner's network listener: where the shared UI is served on the owner's network, and the
// certificate it serves with. Only the desktop reaches these routes, as with the accounts.

function When({ iso }: { iso: string }) {
  const date = new Date(iso);
  return <time dateTime={iso}>{Number.isNaN(date.getTime()) ? iso : date.toLocaleString()}</time>;
}

export function Listener({ client, onClose }: { client: AdaClient; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [status, setStatus] = useState<ListenerStatus | null>(null);
  const [error, setError] = useState<Shown | null>(null);
  const [busy, setBusy] = useState("");
  const [enabled, setEnabled] = useState(false);
  const [address, setAddress] = useState("");
  const [name, setName] = useState("");

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

  // The first read is dropped when a change answered before it; a refused change keeps it.
  const answers = useRef(0);
  const show = (next: ListenerStatus, fill: boolean) => {
    ++answers.current;
    setStatus(next);
    if (!fill) return;
    setEnabled(next.enabled);
    setAddress(next.address);
    setName(next.name);
  };

  // Reads the status; a failed read is shown with a way to try again.
  const [reading, setReading] = useState(false);
  // Each read takes a generation; only the latest read's answer, error and end apply.
  const generation = useRef(0);
  const readStatus = () => {
    const gen = ++generation.current;
    const latest = () => gen === generation.current;
    const read = answers.current;
    setReading(true);
    setError(null);
    client
      .listener()
      .then(
        (next) => {
          if (latest() && read === answers.current) show(next, true);
        },
        (e: unknown) => {
          if (latest() && read === answers.current) setError(shown(e));
        },
      )
      .finally(() => {
        if (latest()) setReading(false);
      });
  };

  useEffect(readStatus, [client]);

  const act = async (what: string, fn: () => Promise<void>) => {
    setBusy(what);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(shown(e));
    } finally {
      setBusy("");
    }
  };

  const save = (e: FormEvent) => {
    e.preventDefault();
    void act("save", async () => {
      show(await client.setListener({ enabled, address: address.trim(), name: name.trim() }), true);
    });
  };
  // Unsaved fields stay as typed: installing a certificate changes only the pair.
  const install = () =>
    void act("certificate", async () => {
      const next = await client.installListenerCertificate();
      if (next !== null) show(next, false);
    });

  const idle = busy === "";
  // Nothing changes before the first read: neither a save of empty fields nor an install.
  const ready = idle && status !== null;
  const certificate = status?.certificate ?? null;

  return (
    <dialog
      ref={dialog}
      className="import accounts"
      aria-labelledby="listener-title"
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="import-head">
        <h2 id="listener-title">Network access</h2>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div className="import-body">
        {error && <ErrorLine error={error} alert={false} />}
        {status === null ? (
          error ? (
            <button type="button" className="quiet" disabled={reading} onClick={readStatus}>
              Try again
            </button>
          ) : (
            <Skeleton rows={2} label="Loading the listener" />
          )
        ) : (
          <p role="status" aria-label="Listener state">
            {status.state === "serving" ? `Serving at ${status.origin}` : status.message === "" ? "Off" : `Off. ${status.message}`}
          </p>
        )}

        <form className="task-form" onSubmit={save} aria-labelledby="listener-setting">
          <h3 id="listener-setting">Setting</h3>
          <label className="inline">
            <input type="checkbox" checked={enabled} disabled={!idle} onChange={(e) => setEnabled(e.target.checked)} />
            Enabled
          </label>
          <label>
            Address
            <input type="text" value={address} placeholder="192.168.1.20:8443" spellCheck={false} disabled={!idle} onChange={(e) => setAddress(e.target.value)} />
          </label>
          <label>
            Name
            <input type="text" value={name} placeholder="home.adanext.si" spellCheck={false} disabled={!idle} onChange={(e) => setName(e.target.value)} />
          </label>
          <div>
            {/* Saving before the setting is read would replace it with empty fields. */}
            <BusyButton type="submit" busy={busy === "save"} disabled={!ready}>
              Save
            </BusyButton>
          </div>
        </form>

        <section aria-labelledby="listener-certificate">
          <h3 id="listener-certificate">Certificate</h3>
          {certificate === null ? (
            <p className="empty">No certificate installed.</p>
          ) : (
            <>
              <p>Names: {certificate.names.join(", ")}</p>
              <p>
                Expires <When iso={certificate.not_after} />
              </p>
            </>
          )}
          <BusyButton busy={busy === "certificate"} className="quiet" disabled={!ready} onClick={install}>
            Choose certificate and key…
          </BusyButton>
        </section>

        <p className="hint" role="note">
          The name, such as home.adanext.si, must resolve to the address on your own network. The certificate comes from files for now: a PEM certificate chain and its private key, which Ada copies into its own configuration. The listener serves HTTPS only and never serves plain HTTP.
        </p>
      </div>
    </dialog>
  );
}
