import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type FormEvent } from "react";
import type { Account, AdaClient, Member, MemberRole, Mode, ProjectSummary } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { BusyButton, Skeleton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "../workspace/errors";

// A project's members and its default mode, on the desktop only: adad serves these routes on
// its local channel to the owner and answers local_only everywhere else.

const modes: Mode[] = ["readonly", "auto", "yolo"];
type Confirm = { kind: "role"; member: Member; role: MemberRole } | { kind: "remove"; member: Member };

function ConfirmPanel({ confirm, project, busy, onConfirm, onCancel }: { confirm: Confirm; project: string; busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  const name = confirm.member.name;
  return (
    <div className="note confirm" role="group" aria-label="Confirm">
      {confirm.kind === "role" ? (
        <p>
          Make <Marked text={name} /> {confirm.role === "owner" ? "an owner" : "a member"} of <Marked text={project} />? A change of role signs {name} out everywhere: every session of the account ends at once.
        </p>
      ) : (
        <p>
          Remove <Marked text={name} /> from <Marked text={project} />? {name}'s attempts waiting for a decision are denied and cancelled with the reason “membership revoked”, and {name}'s other queued and running attempts in the project, tasks in verification included, are cancelled with the reason “actor removed from the project”. {name} can then read nothing of the project.
        </p>
      )}
      <div className="actions">
        <BusyButton busy={busy} className="danger" onClick={onConfirm}>
          {confirm.kind === "role" ? "Yes, change the role" : `Yes, remove ${name}`}
        </BusyButton>
        {/* Focus lands on the way out, so Enter alone never confirms. */}
        <button type="button" className="quiet" autoFocus onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

export function ProjectSettings({ client, project, onClose, onChanged }: { client: AdaClient; project: ProjectSummary; onClose: () => void; onChanged: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const title = useId();
  const [members, setMembers] = useState<Member[] | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [error, setError] = useState<Shown | null>(null);
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [adding, setAdding] = useState("");
  const [role, setRole] = useState<MemberRole>("member");
  const [mode, setMode] = useState<Mode>(project.default_mode);
  const [saved, setSaved] = useState("");

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
    const [listed, all] = await Promise.all([client.members(project.id), client.listAccounts()]);
    setMembers(listed);
    setAccounts(all);
  }, [client, project.id]);
  useEffect(() => {
    load().catch((e: unknown) => setError(shown(e)));
  }, [load]);

  const act = async (what: string, fn: () => Promise<void>) => {
    setBusy(what);
    setError(null);
    setSaved("");
    try {
      await fn();
    } catch (e) {
      setError(shown(e));
    } finally {
      setBusy("");
    }
  };

  const candidates = accounts.filter((a) => !a.disabled && !(members ?? []).some((m) => m.user === a.id));
  const chosen = candidates.some((a) => a.id === adding) ? adding : (candidates[0]?.id ?? "");
  const add = (e: FormEvent) => {
    e.preventDefault();
    if (chosen === "") return;
    void act("add", async () => setMembers(await client.setMember(project.id, chosen, role)));
  };
  const confirmed = (c: Confirm) =>
    void act("confirm", async () => {
      try {
        setMembers(c.kind === "role" ? await client.setMember(project.id, c.member.user, c.role) : await client.removeMember(project.id, c.member.user));
      } finally {
        setConfirm(null);
      }
    });
  const saveMode = (e: FormEvent) => {
    e.preventDefault();
    void act("mode", async () => {
      const result = await client.setDefaultMode(project.id, mode);
      setMode(result.default_mode);
      setSaved(`The default mode is now ${result.default_mode}.`);
      onChanged();
    });
  };
  const idle = busy === "" && confirm === null;

  return (
    <dialog
      ref={dialog}
      className="import accounts"
      aria-labelledby={title}
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="import-head">
        <h2 id={title}>
          Settings of <Marked text={project.name} />
        </h2>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div className="import-body">
        {error && <ErrorLine error={error} alert={false} />}
        {confirm && <ConfirmPanel confirm={confirm} project={project.name} busy={busy === "confirm"} onConfirm={() => confirmed(confirm)} onCancel={() => setConfirm(null)} />}

        <section aria-labelledby={`${title}-members`}>
          <h3 id={`${title}-members`}>Members</h3>
          {members === null ? (
            error ? null : <Skeleton rows={2} label="Loading members" />
          ) : (
            <table className="tree accounts-table" aria-labelledby={`${title}-members`}>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Role</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {members.map((m) => {
                  const other: MemberRole = m.role === "owner" ? "member" : "owner";
                  return (
                    <tr key={m.user} data-member={m.user}>
                      <td>
                        <Marked text={m.name} />
                      </td>
                      <td>{m.role}</td>
                      <td className="row-actions">
                        <button type="button" className="quiet" disabled={!idle} aria-label={`Make ${m.name} ${other === "owner" ? "an owner" : "a member"}`} onClick={() => setConfirm({ kind: "role", member: m, role: other })}>
                          {other === "owner" ? "Make owner" : "Make member"}
                        </button>
                        <button type="button" className="quiet" disabled={!idle} aria-label={`Remove ${m.name}`} onClick={() => setConfirm({ kind: "remove", member: m })}>
                          Remove
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
          <p className="hint">An owner of the project may deny another member's start that waits for a decision. A project keeps at least one owner.</p>
        </section>

        <form className="task-form" onSubmit={add} aria-labelledby={`${title}-add`}>
          <h3 id={`${title}-add`}>Add a member</h3>
          {candidates.length === 0 ? (
            <p className="empty">Every account is already a member. Create accounts under Accounts.</p>
          ) : (
            <>
              <label>
                Account
                <select value={chosen} onChange={(e) => setAdding(e.target.value)}>
                  {candidates.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Role
                <select value={role} onChange={(e) => setRole(e.target.value as MemberRole)}>
                  <option value="member">member</option>
                  <option value="owner">owner</option>
                </select>
              </label>
              <div>
                <BusyButton type="submit" busy={busy === "add"} disabled={!idle}>
                  Add to the project
                </BusyButton>
              </div>
            </>
          )}
        </form>

        <form className="task-form" onSubmit={saveMode} aria-labelledby={`${title}-mode`}>
          <h3 id={`${title}-mode`}>Default mode</h3>
          <p className="hint">
            A start in a mode above the default waits for its actor to allow it before it runs; a start at or below the default runs without asking. Lowering the default cancels nothing: queued starts are judged again when their turn comes.
          </p>
          <label>
            The project's default mode
            <select value={mode} onChange={(e) => setMode(e.target.value as Mode)}>
              {modes.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>
          <div>
            <BusyButton type="submit" busy={busy === "mode"} disabled={!idle}>
              Save the default mode
            </BusyButton>
          </div>
          <p role="status">{saved}</p>
        </form>
      </div>
    </dialog>
  );
}
