import { useLayoutEffect, useRef, useState } from "react";
import { AdaError, type AdaClient, type Blocker, type Capabilities, type ChosenFolder, type ErrorDetails, type ExecutionPolicy, type Inspection, type NewTask, type Snapshot } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { BusyButton } from "../motion/parts";
import { PathText } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "./errors";
import { sensitive } from "./sensitive";
import { TaskForm } from "./TaskForm";

// Import a repository, then create its first task (plan section 3, "Import"). The folder comes
// from the desktop's own dialog, opened by main: the page never types a path. Then the
// inspection, with its blockers and the provider configuration it found; the uncommitted work
// offered for the new branch, untracked files chosen one by one and credentials marked; the
// snapshot; and the task.

type Step = "choose" | "inspect" | "task";

const blockerNames: Record<string, string> = {
  split_index: "The index is split",
  sparse_checkout: "Sparse checkout is on",
  merge_in_progress: "A merge is in progress",
  rebase_in_progress: "A rebase is in progress",
  cherry_pick_in_progress: "A cherry-pick is in progress",
  revert_in_progress: "A revert is in progress",
  bisect_in_progress: "A bisect is in progress",
  unmerged_entries: "There are unmerged paths",
  submodule: "Submodules are not supported yet",
  git_lfs: "Git LFS objects are not supported",
  escaping_link: "A link points outside the repository",
  unc_path: "The repository is on a network path",
  assume_unchanged: "Paths are marked assume-unchanged or skip-worktree",
};

// A snapshot takes at most this many untracked files (internal/api maxSelected).
const maxUntracked = 1000;

function BlockerList({ blockers }: { blockers: Blocker[] }) {
  return (
    <>
      {blockers.map((b) => (
        <div key={b.code} className="blocker">
          <p>
            <strong>{blockerNames[b.code] ?? b.code}</strong> <code>{b.code}</code>
          </p>
          {b.paths.length > 0 && (
            <ul>
              {b.paths.map((p) => (
                <li key={p}>
                  <PathText path={p} />
                </li>
              ))}
              {b.count > b.paths.length && <li>and {b.count - b.paths.length} more</li>}
            </ul>
          )}
        </div>
      ))}
    </>
  );
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] ?? "";
}

export function ImportFlow({
  client,
  matrix,
  onClose,
  onProject,
  onCreated,
}: {
  client: AdaClient;
  matrix: Capabilities | null;
  onClose: () => void;
  onProject: (id: string, name: string) => void;
  onCreated: (input: NewTask, policy: ExecutionPolicy) => Promise<void>;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [step, setStep] = useState<Step>("choose");
  const [folder, setFolder] = useState<ChosenFolder | null>(null);
  const [inspection, setInspection] = useState<Inspection | null>(null);
  const [name, setName] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [project, setProject] = useState<{ id: string; name: string } | null>(null);
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState<Shown | null>(null);
  // What a refusal carried beside its code: the blockers of a refused capture, a refused path.
  const [details, setDetails] = useState<ErrorDetails | undefined>(undefined);
  const [includeChanges, setIncludeChanges] = useState(true);
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

  const act = async (what: string, fn: () => Promise<void>) => {
    setBusy(what);
    setError(null);
    setDetails(undefined);
    try {
      await fn();
    } catch (e) {
      setError(shown(e));
      if (e instanceof AdaError) setDetails(e.details);
    } finally {
      setBusy("");
    }
  };

  const choose = () =>
    act("choose", async () => {
      const chosen = await client.chooseFolder();
      if (!chosen) return;
      setFolder(chosen);
      setName(basename(chosen.path));
      setProject(null);
      setSnapshot(null);
      setNotice("");
      const found = await client.inspectFolder(chosen.handle);
      setInspection(found);
      setIncludeChanges(true);
      // Over the limit nothing is preselected: the owner chooses what the snapshot needs.
      setSelected(new Set(found.status.untracked.length > maxUntracked ? [] : found.status.untracked.filter((p) => !sensitive(p))));
      setStep("inspect");
    });

  const take = () =>
    act("snapshot", async () => {
      if (!folder) return;
      if (selected.size > maxUntracked) throw new Error(`A snapshot takes at most ${maxUntracked} untracked files; ${selected.size} are chosen.`);
      let target = project;
      if (!target) {
        try {
          const imported = await client.importFolder(folder.handle, name.trim());
          target = { id: imported.id, name: imported.name };
        } catch (e) {
          if (!(e instanceof AdaError && e.code === "already_imported" && e.details?.project)) throw e;
          target = { id: e.details.project, name: name.trim() || basename(folder.path) };
          setNotice(`This repository is already imported as project ${e.details.project}; the new task joins it.`);
        }
        setProject(target);
        onProject(target.id, target.name);
      }
      const untracked = [...selected];
      const taken = await client.snapshot(folder.handle, { untracked, include_sensitive: untracked.filter(sensitive), include_changes: includeChanges });
      setSnapshot(taken);
      setStep("task");
    });

  const toggle = (path: string) =>
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });

  const blocked = (inspection?.blockers.length ?? 0) > 0;
  const provider = inspection?.findings.filter((f) => f.code === "provider_configuration") ?? [];
  const providerMore = provider.reduce((n, f) => n + Math.max(0, f.count - f.paths.length), 0);
  const uncommitted = inspection ? inspection.status.staged.length + inspection.status.unstaged.length + inspection.status.untracked.length : 0;

  return (
    <dialog
      ref={dialog}
      className="import"
      aria-labelledby="import-title"
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="import-head">
        <h2 id="import-title">Import a repository</h2>
        <ol className="steps" aria-label="Steps">
          <li aria-current={step === "choose" ? "step" : undefined}>Choose</li>
          <li aria-current={step === "inspect" ? "step" : undefined}>Inspect</li>
          <li aria-current={step === "task" ? "step" : undefined}>Task</li>
        </ol>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div className="import-body">
        {error && (
          <div className="error-block">
            <ErrorLine error={error} alert={false} />
            {details?.path !== undefined && (
              <p className="error">
                <PathText path={details.path} />
              </p>
            )}
            {details?.blockers && details.blockers.length > 0 && (
              <div className="blockers" role="group" aria-label="Refused because">
                <BlockerList blockers={details.blockers} />
              </div>
            )}
          </div>
        )}
        {step === "choose" && (
          <section>
            <p>Ada reads the repository without running any of its code, copies it into its own clone, and never changes your checkout.</p>
            <BusyButton busy={busy === "choose"} onClick={() => void choose()}>
              Choose a folder
            </BusyButton>
          </section>
        )}
        {step === "inspect" && inspection && folder && (
          <section className="inspect">
            <p className="inspected">
              <PathText path={folder.path} /> on{" "}
              {inspection.identity.detached ? (
                <>
                  a detached head <code>{inspection.identity.head.slice(0, 12)}</code>
                </>
              ) : (
                <code>{inspection.identity.branch || "an unborn branch"}</code>
              )}
            </p>
            {blocked && (
              <div className="blockers" role="group" aria-label="Blockers">
                <h3>It cannot be imported as it stands</h3>
                <BlockerList blockers={inspection.blockers} />
                <BusyButton busy={busy === "choose"} onClick={() => void choose()}>
                  Choose another folder
                </BusyButton>
              </div>
            )}
            {provider.length > 0 && (
              <div className="note note-warn" role="group" aria-label="Provider configuration">
                <p>The repository holds provider configuration or hooks. The agent's CLI loads them from the worktree, and hooks run even in readonly mode.</p>
                <ul>
                  {provider.flatMap((f) => f.paths).map((p) => (
                    <li key={p}>
                      <PathText path={p} />
                    </li>
                  ))}
                  {providerMore > 0 && <li>and {providerMore} more</li>}
                </ul>
              </div>
            )}
            {!blocked && (
              <>
                <fieldset className="uncommitted">
                  <legend>Uncommitted changes</legend>
                  {uncommitted === 0 ? (
                    <p className="empty">The checkout has no uncommitted changes.</p>
                  ) : (
                    <>
                      {inspection.status.staged.length + inspection.status.unstaged.length > 0 && (
                        <>
                          <label className="inline">
                            <input type="checkbox" checked={includeChanges} onChange={(e) => setIncludeChanges(e.target.checked)} />
                            Include these changes in the new feature branch
                          </label>
                          <p>{includeChanges ? "The staged and unstaged changes are carried as they are:" : "The staged and unstaged changes stay out of the new branch, which starts from the commit:"}</p>
                          <ul className="carried">
                            {[...inspection.status.staged.map((c) => ({ ...c, where: "staged" })), ...inspection.status.unstaged.map((c) => ({ ...c, where: "unstaged" }))].map((c) => (
                              <li key={`${c.where}:${c.path}`}>
                                <span className="tag">{c.where}</span> <PathText path={c.path} />
                              </li>
                            ))}
                          </ul>
                        </>
                      )}
                      {inspection.status.untracked.length > 0 && <p>Untracked files, each only if chosen:</p>}
                      {inspection.status.untracked.length > maxUntracked && (
                        <p className="note">There are {inspection.status.untracked.length} untracked files and a snapshot takes at most {maxUntracked}, so none is chosen: choose the ones the task needs.</p>
                      )}
                      {inspection.status.untracked.map((p) => (
                        <label key={p} className="inline untracked" data-sensitive={sensitive(p) || undefined}>
                          <input type="checkbox" checked={selected.has(p)} onChange={() => toggle(p)} />
                          <PathText path={p} />
                          {sensitive(p) && <span className="tag tag-warn">looks like a credential</span>}
                        </label>
                      ))}
                    </>
                  )}
                </fieldset>
                <label>
                  Project name
                  <input value={name} maxLength={200} onChange={(e) => setName(e.target.value)} disabled={project !== null} />
                </label>
                <BusyButton busy={busy === "snapshot"} onClick={() => void take()}>
                  Import and take the snapshot
                </BusyButton>
              </>
            )}
          </section>
        )}
        {step === "task" && project && snapshot && (
          <section>
            {notice && <p className="note">{notice}</p>}
            <p>
              Snapshot <code>{snapshot.id.slice(0, 12)}</code> of <code>{snapshot.base_commit.slice(0, 12)}</code>
              {snapshot.untracked.length > 0 && ` with ${snapshot.untracked.length} untracked ${snapshot.untracked.length === 1 ? "file" : "files"}`}.
            </p>
            <TaskForm
              matrix={matrix}
              project={project}
              snapshot={snapshot.id}
              busy={busy === "create"}
              onCreate={(input, policy) => void act("create", () => onCreated(input, policy))}
            />
          </section>
        )}
      </div>
    </dialog>
  );
}
