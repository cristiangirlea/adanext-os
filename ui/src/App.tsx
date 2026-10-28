import { useCallback, useEffect, useRef, useState, type CSSProperties, type FormEvent } from "react";
import { AdaError, type AdaClient, type AttemptEvent, type ExecutionPolicy, type Task, type TaskState, type StopResult } from "./client/api";
import { setMotion, useMotionOn } from "./motion/effects";
import { BusyButton, Skeleton } from "./motion/parts";
import { CommandPalette, type QuickAction } from "./palette/CommandPalette";
import { ResizeHandle } from "./resize/ResizeHandle";
import { currentTheme, setTheme, themeNames, themes, type Theme } from "./theme/theme";

// The one view of M2.4: the task list with each task's state, and the selected task's attempt
// events streaming in. The layout of docs/plan.md section 3 comes with M4.
//
// The coordinator owns a task's state and revision: once started, the executor moves it on its
// own, and an engine event says nothing authoritative about the task. So the view reads the task
// back (getTask) whenever its stream delivers or reconnects, after a stale-revision refusal, and
// on a poll while any task is in a state the executor advances by itself.

interface Shown {
  code: string;
  message: string;
  requestId?: string;
}

const described: Record<string, string> = {
  coordinator_unreachable: "coordinator unreachable: adad is not running, or the shell was not told its pipe or socket",
  bridge_unavailable: "this page is not running in the Ada desktop shell",
  bad_response: "the coordinator answered with something the shell could not read",
};

// The states the coordinator leaves on its own; the others change only on a request from here.
const activeStates = new Set<TaskState>(["preparing", "running", "verifying", "cancel_requested", "interrupted"]);
const pollMs = 2000;
const refreshLimit = 4;
// The task list's width limits in pixels; the handle's step is the resize component's default.
const listWidth = { start: 380, min: 260, max: 720 };

function shown(error: unknown): Shown {
  if (error instanceof AdaError) return { code: error.code, message: described[error.code] ?? error.message, requestId: error.requestId };
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
  const [stopResult, setStopResult] = useState<StopResult | null>(null);
  const quitting = useRef(false);
  const [project, setProject] = useState("");
  const [objective, setObjective] = useState("");
  const [policy, setPolicy] = useState<ExecutionPolicy>({ backend: "claude", model: "", effort: "", mode: "readonly" });
  const [loaded, setLoaded] = useState(false);
  const [pressed, setPressed] = useState<string | null>(null);
  const [theme, setThemeShown] = useState<Theme>(currentTheme);
  const motionOn = useMotionOn();
  const [width, setWidth] = useState(listWidth.start);
  const objectiveInput = useRef<HTMLInputElement>(null);

  const selected = tasks.find((task) => task.id === selectedId) ?? null;
  const attemptId = selected?.attempt?.id ?? (selected ? attempts[selected.id] : undefined);

  // A reply never moves a task backwards: a refresh sent before a request may answer after it.
  const replace = useCallback((task: Task) => {
    setTasks((current) => current.map((t) => (t.id === task.id && task.revision >= t.revision ? task : t)));
  }, []);

  // refresh reads one task back from the coordinator: one read per task and refreshLimit in all
  // are in flight, the rest wait their turn in order. A trigger during a task's read asks for one
  // more read after it, since that answer may predate what triggered it. A failure is shown; a
  // success clears nothing.
  const reading = useRef(new Set<string>());
  const again = useRef(new Set<string>());
  const waiting = useRef<string[]>([]);
  const refresh = useCallback(
    async (id: string) => {
      if (reading.current.has(id)) {
        again.current.add(id);
        return;
      }
      if (reading.current.size >= refreshLimit) {
        if (!waiting.current.includes(id)) waiting.current.push(id);
        return;
      }
      reading.current.add(id);
      try {
        replace(await client.getTask(id));
      } catch (e) {
        if (!quitting.current) setError(shown(e));
      } finally {
        reading.current.delete(id);
        if (again.current.delete(id) && !waiting.current.includes(id)) waiting.current.push(id);
        const next = waiting.current.shift();
        if (next !== undefined) void refresh(next);
      }
    },
    [client, replace],
  );

  // run performs one request and shows its refusal. After stale_revision the coordinator has
  // moved the task on; it is read back so the next request carries the current revision. A
  // named button shows busy until its request answers.
  const run = useCallback(
    async (action: () => Promise<void>, taskId?: string, button?: string) => {
      setError(null);
      if (button) setPressed(button);
      try {
        await action();
      } catch (e) {
        setError(shown(e));
        if (taskId !== undefined && e instanceof AdaError && e.code === "stale_revision") void refresh(taskId);
      } finally {
        if (button) setPressed((current) => (current === button ? null : current));
      }
    },
    [refresh],
  );

  useEffect(() => {
    void run(async () => {
      try {
        setTasks(await client.listTasks());
      } finally {
        setLoaded(true);
      }
    });
  }, [client, run]);

  const quit = useCallback(() => {
    if (quitting.current) return;
    quitting.current = true;
    void run(async () => {
      try { setStopResult(await client.stopCoordinator()); }
      catch (e) { quitting.current = false; throw e; }
    });
  }, [client, run]);
  useEffect(() => client.onQuitRequested(quit), [client, quit]);
  // The result has been rendered before main waits for exit and closes the shell.
  useEffect(() => {
    if (stopResult) void run(() => client.finishQuit());
  }, [client, stopResult, run]);

  useEffect(() => {
    if (!attemptId || selectedId === null) return;
    const taskId = selectedId;
    setEvents([]);
    setStream("");
    const unsubscribe = client.subscribeEvents(attemptId, (message) => {
      switch (message.type) {
        case "event":
          setEvents((current) => [...current, message.event]);
          void refresh(taskId);
          break;
        case "reset":
          setEvents([]);
          break;
        case "status":
          setStream(message.code ? `${message.status} (${message.code})` : message.status);
          if (message.status === "connected") void refresh(taskId);
          break;
      }
    });
    return () => {
      unsubscribe();
      setEvents([]);
      setStream("");
    };
  }, [client, attemptId, selectedId, refresh]);

  // While any task is in a state the executor advances on its own, read those tasks back every
  // pollMs; the poll stops once none is.
  const activeIds = tasks
    .filter((task) => activeStates.has(task.state))
    .map((task) => task.id)
    .join(",");
  useEffect(() => {
    if (activeIds === "" || stopResult) return;
    const ids = activeIds.split(",");
    const timer = setInterval(() => {
      for (const id of ids) void refresh(id);
    }, pollMs);
    return () => clearInterval(timer);
  }, [activeIds, refresh, stopResult]);

  const create = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      const task = await client.createTask({ project, objective });
      setTasks((current) => [...current, task]);
      setObjective("");
      setSelectedId(task.id);
    }, undefined, "create");
  };

  const select = (id: string) => setSelectedId(id);

  const chooseTheme = (next: Theme) => {
    setTheme(next);
    setThemeShown(next);
  };
  const otherTheme = themes.find((t) => t !== theme) ?? "light";
  const actions: QuickAction[] = [
    { id: "new-task", label: "Create a task", run: () => objectiveInput.current?.focus() },
    { id: "theme", label: `Switch to the ${themeNames[otherTheme].toLowerCase()} theme`, run: () => chooseTheme(otherTheme) },
    { id: "motion", label: motionOn ? "Turn motion off" : "Turn motion on", run: () => setMotion(!motionOn) },
  ];

  return (
    <main className="ada" style={{ "--tasks-width": `${width}px` } as CSSProperties}>
      <div className="tasks-pane">
        <section className="tasks" id="task-list" aria-label="Tasks">
          <form className="create" onSubmit={create}>
            <label>
              Project
              <input value={project} onChange={(e) => setProject(e.target.value)} required />
            </label>
            <label>
              Objective
              <input ref={objectiveInput} value={objective} onChange={(e) => setObjective(e.target.value)} required />
            </label>
            <BusyButton type="submit" busy={pressed === "create"}>
              Create
            </BusyButton>
          </form>
          {!loaded ? (
            <Skeleton rows={4} label="Loading tasks" />
          ) : tasks.length === 0 ? (
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
        <ResizeHandle
          side="e"
          size={{ width, height: 0 }}
          min={{ width: listWidth.min, height: 0 }}
          max={{ width: listWidth.max, height: 0 }}
          onResize={(size) => setWidth(size.width)}
          label="Resize the task list"
          controls="task-list"
        />
      </div>

      <section className="detail" aria-label="Selected task">
        {error && (
          <p role="alert" className="error">
            <code>{error.code}</code> {error.message}
            {error.requestId && (
              <>
                {" "}
                (request <code>{error.requestId}</code>)
              </>
            )}
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
              {selected.attempt?.queue_position !== undefined && <p>Queue position {selected.attempt.queue_position}</p>}
            </header>
            <div className="actions">
              <BusyButton busy={pressed === "ready"} disabled={selected.state !== "draft"} onClick={() => void run(async () => replace(await client.ready(selected.id, selected.revision)), selected.id, "ready")}>
                Ready
              </BusyButton>
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
              <BusyButton
                busy={pressed === "start"}
                disabled={selected.state !== "ready"}
                onClick={() =>
                  void run(async () => {
                    const started = await client.start(selected.id, selected.revision, policy);
                    const { attempt_id, ...task } = started;
                    replace(task);
                    setAttempts((current) => ({ ...current, [task.id]: attempt_id }));
                  }, selected.id, "start")
                }
              >
                Start
              </BusyButton>
              <BusyButton busy={pressed === "cancel"} disabled={selected.state !== "running" && selected.attempt?.state !== "pending"} onClick={() => void run(async () => {
                replace(await client.cancel(selected.id, selected.revision));
                await refresh(selected.id);
              }, selected.id, "cancel")}>
                Cancel
              </BusyButton>
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
              <p className="empty">This task has no attempt yet.</p>
            )}
          </>
        ) : (
          <p className="empty">Select a task.</p>
        )}
      </section>

      <footer className="status">
        <span>{tasks.length} tasks</span>
        <span className="hint">
          <kbd>Ctrl</kbd> <kbd>P</kbd> commands
        </span>
        <label className="theme-switch">
          Theme
          <select value={theme} onChange={(e) => chooseTheme(e.target.value as Theme)}>
            {themes.map((t) => (
              <option key={t} value={t}>
                {themeNames[t]}
              </option>
            ))}
          </select>
        </label>
        {stopResult && <p role="status">{stopResult.reason}: {stopResult.running} running cancellation requested, {stopResult.queued} queued canceled. Waiting for the coordinator to exit.</p>}
        <button type="button" className="quit" disabled={stopResult !== null} onClick={quit}>
          Quit Ada and stop work
        </button>
      </footer>
      <CommandPalette client={client} actions={actions} onSelectTask={select} />
    </main>
  );
}
