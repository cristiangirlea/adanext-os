import { useState } from "react";
import { AdaError, type AdaClient, type Capabilities, type ErrorDetails, type ExecutionPolicy, type NewTask, type ProjectSummary, type Snapshot } from "../client/api";
import { BusyButton } from "../motion/parts";
import { ErrorLine, shown, type Shown } from "./errors";
import { BlockerList } from "./ImportFlow";
import { TaskForm } from "./TaskForm";

// The browser owner's new task: a snapshot of an imported project, taken by the project's id,
// then the task bound to it. The browser has no folder dialog and never sends a host path.
export function SnapshotTask({
  client,
  projects,
  matrix,
  defaultProject,
  busy,
  onCreate,
}: {
  client: AdaClient;
  projects: ProjectSummary[];
  matrix: Capabilities | null;
  defaultProject: string;
  busy: boolean;
  onCreate: (input: NewTask, policy: ExecutionPolicy) => void;
}) {
  // The choice is derived from the current list, so the select and the request always agree.
  const [picked, setProject] = useState(defaultProject);
  const project = projects.some((p) => p.id === picked) ? picked : (projects[0]?.id ?? "");
  const [includeChanges, setIncludeChanges] = useState(true);
  const [taking, setTaking] = useState(false);
  const [error, setError] = useState<Shown | null>(null);
  const [details, setDetails] = useState<ErrorDetails | undefined>(undefined);
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);

  const take = async () => {
    if (project === "" || taking) return;
    setTaking(true);
    setError(null);
    setDetails(undefined);
    try {
      setSnapshot(await client.snapshotProject(project, { untracked: [], include_sensitive: [], include_changes: includeChanges }));
    } catch (e) {
      setError(shown(e));
      if (e instanceof AdaError) setDetails(e.details);
    } finally {
      setTaking(false);
    }
  };

  if (snapshot) {
    const name = projects.find((p) => p.id === snapshot.project)?.name ?? snapshot.project;
    return (
      <>
        <p className="note">
          Snapshot <code>{snapshot.id.slice(0, 12)}</code> of <code>{snapshot.base_commit.slice(0, 12)}</code>.
        </p>
        <TaskForm matrix={matrix} project={{ id: snapshot.project, name }} snapshot={snapshot.id} busy={busy} onCreate={onCreate} autoFocus />
      </>
    );
  }
  return (
    <div className="task-form" role="group" aria-label="Snapshot">
      <p>A task runs on a snapshot of its project's repository. Take one here; untracked files stay out of it, since only the desktop can choose them.</p>
      {projects.length === 0 ? (
        <p className="empty">You are not a member of any project yet.</p>
      ) : (
        <>
          <label>
            Project
            <select value={project} onChange={(e) => setProject(e.target.value)}>
              {projects.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          <label className="inline">
            <input type="checkbox" checked={includeChanges} onChange={(e) => setIncludeChanges(e.target.checked)} />
            Carry the repository's staged and unstaged changes
          </label>
          <div>
            <BusyButton busy={taking} disabled={taking} onClick={() => void take()}>
              Take a snapshot
            </BusyButton>
          </div>
        </>
      )}
      {error && (
        <div className="error-block">
          <ErrorLine error={error} />
          {details?.blockers && details.blockers.length > 0 && (
            <div className="blockers" role="group" aria-label="Blockers">
              <BlockerList blockers={details.blockers} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}
