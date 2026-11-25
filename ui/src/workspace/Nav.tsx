import type { Task } from "../client/api";
import { Skeleton } from "../motion/parts";
import { ErrorLine, type Shown } from "./errors";

// Where the read of the project list stands: the empty message waits for its answer.
export type ProjectsRead = { state: "loading" } | { state: "ok" } | { state: "error"; error: Shown };

// The left navigation: each project with its tasks, the active ones first in the order adad
// listed them, then the finished ones. The projects come from adad's list, those with no task yet
// included; a project known only from a task shows its id. The browser has no import, new task,
// accounts or network access controls, and opens the signed-in account instead.

const finished = new Set(["accepted", "canceled", "failed"]);

export function Nav({
  tasks,
  loaded,
  names,
  projects: listed = [],
  projectsRead = { state: "ok" },
  tasksError = null,
  browser = false,
  selectedId,
  onSelect,
  onImport,
  onNewTask,
  onAccounts,
  onListener,
  onAccount,
}: {
  tasks: Task[];
  loaded: boolean;
  names: Record<string, string>;
  projects?: string[];
  projectsRead?: ProjectsRead;
  // The task read's refusal; null while it is pending or once it succeeded.
  tasksError?: Shown | null;
  browser?: boolean;
  selectedId: string | null;
  onSelect: (id: string) => void;
  onImport: () => void;
  onNewTask: (project?: string) => void;
  onAccounts: () => void;
  onListener: () => void;
  onAccount?: () => void;
}) {
  const projects: string[] = [...listed];
  for (const task of tasks) if (!projects.includes(task.project)) projects.push(task.project);
  for (const id of Object.keys(names)) if (!projects.includes(id)) projects.push(id);

  return (
    <nav className="nav" id="task-list" aria-label="Projects and tasks">
      <div className="nav-head">
        <span className="wordmark">Ada</span>
        {browser ? (
          <button type="button" className="quiet" onClick={onAccount}>
            Your account
          </button>
        ) : (
          <>
            <button type="button" onClick={onImport}>
              Import a repository
            </button>
            <button type="button" className="quiet" onClick={() => onNewTask()}>
              New task
            </button>
            <button type="button" className="quiet" onClick={onAccounts}>
              Accounts
            </button>
            <button type="button" className="quiet" onClick={onListener}>
              Network access
            </button>
          </>
        )}
      </div>
      {projectsRead.state === "loading" && <Skeleton rows={2} label="Loading projects" />}
      {projectsRead.state === "error" && <ErrorLine error={projectsRead.error} alert={false} />}
      {!loaded && <Skeleton rows={4} label="Loading tasks" />}
      {/* The captions show once projects() answers; a task read that failed says nothing of them. */}
      <>
          {loaded && !tasksError && projects.length === 0 && projectsRead.state === "ok" && (
            <p className="empty">{browser ? "You are not a member of any project yet." : "No tasks yet. Import a repository to start one."}</p>
          )}
          {projects.map((project) => {
          const own = tasks.filter((t) => t.project === project);
          const ordered = [...own.filter((t) => !finished.has(t.state)), ...own.filter((t) => finished.has(t.state))];
          return (
            <table key={project} className="project">
              <caption>
                <span className="project-name">{names[project] ?? project}</span>
                {!browser && (
                  <button type="button" className="link add" onClick={() => onNewTask(project)} aria-label={`New task in ${names[project] ?? project}`}>
                    New
                  </button>
                )}
              </caption>
              <thead className="visually-hidden">
                <tr>
                  <th scope="col">Objective</th>
                  <th scope="col">State</th>
                </tr>
              </thead>
              <tbody>
                {ordered.length === 0 && loaded && !tasksError && (
                  <tr className="project-empty">
                    <td colSpan={2}>No tasks yet.</td>
                  </tr>
                )}
                {ordered.map((task) => (
                  <tr
                    key={task.id}
                    data-task={task.id}
                    data-finished={finished.has(task.state) || undefined}
                    className={task.id === selectedId ? "selected" : ""}
                    tabIndex={0}
                    aria-selected={task.id === selectedId}
                    onClick={() => onSelect(task.id)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        onSelect(task.id);
                      }
                    }}
                  >
                    <td>{task.objective}</td>
                    <td>
                      <span className={`state state-${task.state}`}>{task.state}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          );
        })}
      </>
    </nav>
  );
}
