import { useCallback, useEffect, useState, type FormEvent } from "react";
import { AdaError, type AdaClient, type AttemptEvent, type ExecutionPolicy, type Task } from "./client/api";

// The one view of M2.4: the task list with each task's state, and the selected task's attempt
// events streaming in. The layout of docs/plan.md section 3 comes with M4.

interface Shown {
  code: string;
  message: string;
}

const described: Record<string, string> = {
  coordinator_unreachable: "coordinator unreachable: adad is not running, or the shell was not told its pipe or socket",
  bridge_unavailable: "this page is not running in the Ada desktop shell",
};

function shown(error: unknown): Shown {
  if (error instanceof AdaError) return { code: error.code, message: described[error.code] ?? error.message };
  return { code: "internal", message: error instanceof Error ? error.message : String(error) };
}

function payloadText(payload: unknown): string {
  const text = JSON.stringify(payload) ?? "";
  return text.length > 240 ? `${text.slice(0, 240)}…` : text;
}

export function App({ client }: { client: AdaClient }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [attempts, setAttempts] = useState<Record<string, string>>({});
  const [events, setEvents] = useState<AttemptEvent[]>([]);
  const [stream, setStream] = useState<string>("");
  const [error, setError] = useState<Shown | null>(null);
  const [project, setProject] = useState("");
  const [objective, setObjective] = useState("");
  const [policy, setPolicy] = useState<ExecutionPolicy>({ backend: "claude", model: "", effort: "", mode: "readonly" });

  const selected = tasks.find((task) => task.id === selectedId) ?? null;
  const attemptId = selected ? attempts[selected.id] : undefined;

  const replace = useCallback((task: Task) => {
    setTasks((current) => current.map((t) => (t.id === task.id ? task : t)));
  }, []);

  const run = useCallback(async (action: () => Promise<void>) => {
    setError(null);
    try {
      await action();
    } catch (e) {
      setError(shown(e));
    }
  }, []);

  useEffect(() => {
    void run(async () => setTasks(await client.listTasks()));
  }, [client, run]);

  useEffect(() => {
    if (!attemptId) return;
    setEvents([]);
    setStream("");
    const unsubscribe = client.subscribeEvents(attemptId, (message) => {
      switch (message.type) {
        case "event":
          setEvents((current) => [...current, message.event]);
          break;
        case "reset":
          setEvents([]);
          break;
        case "status":
          setStream(message.code ? `${message.status} (${message.code})` : message.status);
          break;
      }
    });
    return () => {
      unsubscribe();
      setEvents([]);
      setStream("");
    };
  }, [client, attemptId]);

  const create = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      const task = await client.createTask({ project, objective });
      setTasks((current) => [...current, task]);
      setObjective("");
      setSelectedId(task.id);
    });
  };

  const select = (id: string) => setSelectedId(id);

  return (
    <main className="ada">
      <section className="tasks" aria-label="Tasks">
        <form className="create" onSubmit={create}>
          <label>
            Project
            <input value={project} onChange={(e) => setProject(e.target.value)} required />
          </label>
          <label>
            Objective
            <input value={objective} onChange={(e) => setObjective(e.target.value)} required />
          </label>
          <button type="submit">Create</button>
        </form>
        {tasks.length === 0 ? (
          <p className="empty">No tasks yet.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Objective</th>
                <th>Project</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {tasks.map((task) => (
                <tr
                  key={task.id}
                  data-task={task.id}
                  className={task.id === selectedId ? "selected" : ""}
                  tabIndex={0}
                  aria-selected={task.id === selectedId}
                  onClick={() => select(task.id)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      select(task.id);
                    }
                  }}
                >
                  <td>{task.objective}</td>
                  <td>{task.project}</td>
                  <td>
                    <span className={`state state-${task.state}`}>{task.state}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="detail" aria-label="Selected task">
        {error && (
          <p role="alert" className="error">
            <code>{error.code}</code> {error.message}
          </p>
        )}
        {selected ? (
          <>
            <header>
              <h2>{selected.objective}</h2>
              <p>
                <span className={`state state-${selected.state}`}>{selected.state}</span> revision {selected.revision}
                {attemptId && (
                  <>
                    {" "}
                    attempt <code>{attemptId}</code>
                  </>
                )}
              </p>
            </header>
            <div className="actions">
              <button type="button" disabled={selected.state !== "draft"} onClick={() => void run(async () => replace(await client.ready(selected.id, selected.revision)))}>
                Ready
              </button>
              <label>
                Backend
                <input value={policy.backend} onChange={(e) => setPolicy({ ...policy, backend: e.target.value })} />
              </label>
              <label>
                Model
                <input value={policy.model} onChange={(e) => setPolicy({ ...policy, model: e.target.value })} />
              </label>
              <label>
                Mode
                <select value={policy.mode} onChange={(e) => setPolicy({ ...policy, mode: e.target.value as ExecutionPolicy["mode"] })}>
                  <option value="readonly">readonly</option>
                  <option value="auto">auto</option>
                  <option value="yolo">yolo</option>
                </select>
              </label>
              <button
                type="button"
                disabled={selected.state !== "ready"}
                onClick={() =>
                  void run(async () => {
                    const started = await client.start(selected.id, selected.revision, policy);
                    const { attempt_id, ...task } = started;
                    replace(task);
                    setAttempts((current) => ({ ...current, [task.id]: attempt_id }));
                  })
                }
              >
                Start
              </button>
              <button type="button" disabled={selected.state !== "running"} onClick={() => void run(async () => replace(await client.cancel(selected.id, selected.revision)))}>
                Cancel
              </button>
            </div>
            <h3>
              Events {stream && <span className="stream">{stream}</span>}
            </h3>
            {attemptId ? (
              events.length === 0 ? (
                <p className="empty">No events yet.</p>
              ) : (
                <ol className="events">
                  {events.map((event) => (
                    <li key={event.seq} data-seq={event.seq}>
                      <span className="seq">{event.seq}</span>
                      <span className="kind">{event.kind}</span>
                      <code className="payload">{payloadText(event.payload)}</code>
                    </li>
                  ))}
                </ol>
              )
            ) : (
              <p className="empty">Events stream once this task is started from here.</p>
            )}
          </>
        ) : (
          <p className="empty">Select a task.</p>
        )}
      </section>

      <footer className="status">
        <span>{tasks.length} tasks</span>
        <button type="button" className="quit" onClick={() => void run(() => client.stopCoordinator())}>
          Quit Ada and stop work
        </button>
      </footer>
    </main>
  );
}
