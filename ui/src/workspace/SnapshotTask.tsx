import { useEffect, useState } from "react";
import { AdaError, type AdaClient, type Capabilities, type ErrorDetails, type ExecutionPolicy, type NewTask, type ProjectSummary, type Snapshot } from "../client/api";
import { BusyButton } from "../motion/parts";
import { ErrorLine, shown, type Shown } from "./errors";
import { BlockerList } from "./ImportFlow";
import { sensitive } from "./sensitive";
import { PathText } from "../text/Marked";
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

  const [files, setFiles] = useState<{ project: string; paths: string[]; selected: string[] } | null>(null);
  useEffect(() => {
    let live = true;
    setFiles(null);
    setError(null);
    setDetails(undefined);
    if (project) client.projectUntracked(project).then(paths => {
      if (live) setFiles({ project, paths, selected: paths.length > 1000 ? [] : paths.filter(p => !sensitive(p)) });
    }, e => {
      if (live) {
        setError(shown(e));
        if (e instanceof AdaError) setDetails(e.details);
      }
    });
    return () => { live = false; };
  }, [client, project]);
  const loaded = files?.project === project ? files : null;

  const take = async () => {
    if (!loaded || loaded.selected.length > 1000 || taking) return;
    setTaking(true);
    setError(null);
    setDetails(undefined);
    try {
      setSnapshot(await client.snapshotProject(project, { untracked: loaded.selected, include_sensitive: loaded.selected.filter(sensitive), include_changes: includeChanges }));
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
      <p>A task runs on a snapshot of its project's repository. Choose the files to include.</p>
      {projects.length === 0 ? (
        <p className="empty">You are not a member of any project yet.</p>
      ) : (
        <>
          <label>
            Project
            <select disabled={taking} value={project} onChange={(e) => setProject(e.target.value)}>
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
          {loaded && loaded.paths.length > 0 && <fieldset disabled={taking}>
            <legend>Untracked files</legend>
            {loaded.paths.length > 1000 && <p>Choose up to 1,000 files.</p>}
            {loaded.paths.map(path => <label className="inline" key={path}>
              <input type="checkbox" checked={loaded.selected.includes(path)} onChange={e => {
                const selected = e.target.checked ? [...loaded.selected, path] : loaded.selected.filter(p => p !== path);
                setFiles({ ...loaded, selected });
              }} /><PathText path={path} />{sensitive(path) && <span className="tag tag-warn">looks like a credential</span>}
            </label>)}
          </fieldset>}
          <div>
            <BusyButton busy={taking} disabled={taking || !loaded || loaded.selected.length > 1000} onClick={() => void take()}>
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
