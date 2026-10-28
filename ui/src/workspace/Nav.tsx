import type { Task } from "../client/api";
import { Skeleton } from "../motion/parts";

// The left navigation: each project with its tasks, the active ones first in the order adad
// listed them, then the finished ones. A project's name is known for those imported in this
// window; the others show their id.

const finished = new Set(["accepted", "canceled", "failed"]);

export function Nav({
  tasks,
  loaded,
  names,
  selectedId,
  onSelect,
  onImport,
  onNewTask,
}: {
  tasks: Task[];
  loaded: boolean;
  names: Record<string, string>;
  selectedId: string | null;
  onSelect: (id: string) => void;
  onImport: () => void;
  onNewTask: (project?: string) => void;
}) {
  const projects: string[] = [];
  for (const task of tasks) if (!projects.includes(task.project)) projects.push(task.project);
  for (const id of Object.keys(names)) if (!projects.includes(id)) projects.push(id);

  return (
    <nav className="nav" id="task-list" aria-label="Projects and tasks">
      <div className="nav-head">
        <span className="wordmark">Ada</span>
        <button type="button" onClick={onImport}>
          Import a repository
        </button>
        <button type="button" className="quiet" onClick={() => onNewTask()}>
          New task
        </button>
      </div>
      {!loaded ? (
        <Skeleton rows={4} label="Loading tasks" />
      ) : projects.length === 0 ? (
        <p className="empty">No tasks yet. Import a repository to start one.</p>
      ) : (
        projects.map((project) => {
          const own = tasks.filter((t) => t.project === project);
          const ordered = [...own.filter((t) => !finished.has(t.state)), ...own.filter((t) => finished.has(t.state))];
          return (
            <table key={project} className="project">
              <caption>
                <span className="project-name">{names[project] ?? project}</span>
                <button type="button" className="link add" onClick={() => onNewTask(project)} aria-label={`New task in ${names[project] ?? project}`}>
                  New
                </button>
              </caption>
              <thead className="visually-hidden">
                <tr>
                  <th scope="col">Objective</th>
                  <th scope="col">State</th>
                </tr>
              </thead>
              <tbody>
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
        })
      )}
    </nav>
  );
}
