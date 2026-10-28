import { useCallback, useEffect, useRef, useState, type CSSProperties } from "react";
import { AdaError, type AdaClient, type AttemptEvent, type Capabilities, type Changes, type ExecutionPolicy, type NewTask, type Task, type TaskState, type StopResult } from "./client/api";
import { setMotion, useMotionOn } from "./motion/effects";
import { BusyButton } from "./motion/parts";
import { CommandPalette, type QuickAction } from "./palette/CommandPalette";
import { ResizeHandle } from "./resize/ResizeHandle";
import { currentTheme, setTheme, themeNames, themes, type Theme } from "./theme/theme";
import { PathText } from "./text/Marked";
import { Activity } from "./workspace/Activity";
import { ErrorLine, shown, type Shown } from "./workspace/errors";
import { ImportFlow } from "./workspace/ImportFlow";
import { Nav } from "./workspace/Nav";
import { defaultPolicy } from "./workspace/policy";
import { PolicyPicker } from "./workspace/PolicyPicker";
import { Review } from "./workspace/Review";
import { TaskForm } from "./workspace/TaskForm";
import { WorkArea } from "./workspace/WorkArea";

// The workspace (plan section 3, "Workspace UI"): projects and tasks on the left, the selected
// task with its review and activity in the middle, its changes, checks and files in a resizable
// work area on the right, and the status bar along the bottom.
//
// The coordinator owns a task's state and revision: once started, the executor moves it on its
// own, and an engine event says nothing authoritative about the task. So the view reads the task
// back (getTask) whenever its stream delivers or reconnects, after a stale-revision refusal, and
// on a poll while any task is in a state the executor advances by itself.

// The states the coordinator leaves on its own; the others change only on a request from here.
const activeStates = new Set<TaskState>(["preparing", "running", "verifying", "cancel_requested", "interrupted"]);
const reviewStates = new Set<TaskState>(["awaiting_review", "accepted", "failed", "canceled"]);
const pollMs = 2000;
const refreshLimit = 4;
// Pane widths in pixels; the handle's step is the resize component's default.
const navWidth = { start: 300, min: 220, max: 520 };
const workWidth = { start: 520, min: 320, max: 1100 };

export function App({ client }: { client: AdaClient }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [attempts, setAttempts] = useState<Record<string, string>>({});
  // The policy each attempt was started with from this window; adad's task answer does not say.
  const [ranWith, setRanWith] = useState<Record<string, ExecutionPolicy>>({});
  // The policy chosen for a task's next start, from its creation form or the picker.
  const [nextPolicy, setNextPolicy] = useState<Record<string, ExecutionPolicy>>({});
  const [events, setEvents] = useState<AttemptEvent[]>([]);
  const [stream, setStream] = useState<string>("");
  const [error, setError] = useState<Shown | null>(null);
  const [stopResult, setStopResult] = useState<StopResult | null>(null);
  const quitting = useRef(false);
  const [matrix, setMatrix] = useState<Capabilities | null>(null);
  const [names, setNames] = useState<Record<string, string>>({});
  const [importing, setImporting] = useState(false);
  const [formProject, setFormProject] = useState("");
  // Counts the "New task" openings: each opens a fresh form with its first field focused.
  const [formOpened, setFormOpened] = useState(0);
  const [reviewed, setReviewed] = useState<Changes | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [pressed, setPressed] = useState<string | null>(null);
  const [theme, setThemeShown] = useState<Theme>(currentTheme);
  const motionOn = useMotionOn();
  const [width, setWidth] = useState(navWidth.start);
  const [work, setWork] = useState(workWidth.start);

  const selected = tasks.find((task) => task.id === selectedId) ?? null;
  const attemptId = selected?.attempt?.id ?? (selected ? attempts[selected.id] : undefined);
  const policy = (selected && nextPolicy[selected.id]) || defaultPolicy(matrix);

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
        const listed = await client.listTasks();
        // A task created or read while the listing was on its way is newer than the listing: keep
        // it, and keep the higher revision of a task both hold.
        setTasks((current) => {
          const merged = listed.map((task) => {
            const held = current.find((t) => t.id === task.id);
            return held && held.revision > task.revision ? held : task;
          });
          return [...merged, ...current.filter((t) => !listed.some((task) => task.id === t.id))];
        });
      } finally {
        setLoaded(true);
      }
    });
  }, [client, run]);

  // The capability matrix feeds every policy picker; without it the pickers offer the modes
  // with no cells and readonly still preselected.
  useEffect(() => {
    let live = true;
    client.capabilities().then(
      (m) => live && setMatrix(m),
      () => {},
    );
    return () => {
      live = false;
    };
  }, [client]);

  const quit = useCallback(() => {
    if (quitting.current) return;
    quitting.current = true;
    void run(async () => {
      try {
        setStopResult(await client.stopCoordinator());
      } catch (e) {
        quitting.current = false;
        throw e;
      }
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

  const created = useCallback(
    async (input: NewTask, chosen: ExecutionPolicy) => {
      const task = await client.createTask(input);
      setTasks((current) => [...current, task]);
      setNextPolicy((current) => ({ ...current, [task.id]: chosen }));
      setSelectedId(task.id);
    },
    [client],
  );

  const select = (id: string) => setSelectedId(id);
  const newTask = (project?: string) => {
    setSelectedId(null);
    setFormProject(project ?? "");
    setFormOpened((n) => n + 1);
  };
  const started = useCallback(
    (task: Task, attempt: string, used: ExecutionPolicy) => {
      replace(task);
      setAttempts((current) => ({ ...current, [task.id]: attempt }));
      setRanWith((current) => ({ ...current, [attempt]: used }));
    },
    [replace],
  );
  const cancel = (task: Task) =>
    void run(
      async () => {
        replace(await client.cancel(task.id, task.revision));
        await refresh(task.id);
      },
      task.id,
      "cancel",
    );
  const cancellable = selected !== null && (selected.state === "running" || selected.state === "verifying" || selected.attempt?.state === "pending");

  const chooseTheme = (next: Theme) => {
    setTheme(next);
    setThemeShown(next);
  };
  const otherTheme = themes.find((t) => t !== theme) ?? "light";
  const actions: QuickAction[] = [
    { id: "import", label: "Import a repository", run: () => setImporting(true) },
    { id: "new-task", label: "Create a task", run: () => newTask() },
    { id: "theme", label: `Switch to the ${themeNames[otherTheme].toLowerCase()} theme`, run: () => chooseTheme(otherTheme) },
    { id: "motion", label: motionOn ? "Turn motion off" : "Turn motion on", run: () => setMotion(!motionOn) },
  ];
  // The attempt's own policy, from adad; until the first read after a start, what this window sent.
  const fromAttempt = selected?.attempt?.backend ? selected.attempt : undefined;
  const used = fromAttempt ? { backend: fromAttempt.backend!, model: fromAttempt.model ?? "", effort: fromAttempt.effort ?? "", mode: fromAttempt.mode ?? "" } : attemptId ? ranWith[attemptId] : undefined;

  return (
    <div className="ada" style={{ "--tasks-width": `${width}px`, "--work-width": `${work}px` } as CSSProperties}>
      <div className="tasks-pane">
        <Nav tasks={tasks} loaded={loaded} names={names} selectedId={selectedId} onSelect={select} onImport={() => setImporting(true)} onNewTask={newTask} />
        <ResizeHandle
          side="e"
          size={{ width, height: 0 }}
          min={{ width: navWidth.min, height: 0 }}
          max={{ width: navWidth.max, height: 0 }}
          onResize={(size) => setWidth(size.width)}
          label="Resize the task list"
          controls="task-list"
        />
      </div>

      <main className="detail" aria-label="Selected task">
        {error && <ErrorLine error={error} />}
        {selected ? (
          <>
            <header className="task-head">
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
              {selected.criteria && <p className="criteria">{selected.criteria}</p>}
              {selected.provider_configuration && selected.provider_configuration.count > 0 && (
                <p className="note note-warn">
                  The worktree holds provider configuration or hooks the CLI loads:{" "}
                  {selected.provider_configuration.paths.map((p, i) => (
                    <span key={p}>
                      {i > 0 && ", "}
                      <PathText path={p} />
                    </span>
                  ))}
                  {selected.provider_configuration.count > selected.provider_configuration.paths.length && ` and ${selected.provider_configuration.count - selected.provider_configuration.paths.length} more`}.
                </p>
              )}
            </header>
            <div className="actions">
              <BusyButton busy={pressed === "ready"} disabled={selected.state !== "draft"} onClick={() => void run(async () => replace(await client.ready(selected.id, selected.revision)), selected.id, "ready")}>
                Ready
              </BusyButton>
              <BusyButton
                busy={pressed === "start"}
                disabled={selected.state !== "ready"}
                onClick={() =>
                  void run(
                    async () => {
                      const result = await client.start(selected.id, selected.revision, policy);
                      const { attempt_id, ...task } = result;
                      started(task, attempt_id, policy);
                    },
                    selected.id,
                    "start",
                  )
                }
              >
                Start
              </BusyButton>
              <BusyButton busy={pressed === "cancel"} className="quiet" disabled={!cancellable} onClick={() => cancel(selected)}>
                Cancel
              </BusyButton>
            </div>
            {(selected.state === "draft" || selected.state === "ready") && (
              <PolicyPicker matrix={matrix} value={policy} onChange={(next) => setNextPolicy((current) => ({ ...current, [selected.id]: next }))} legend="Start with" />
            )}
            {reviewStates.has(selected.state) && selected.attempt && (
              <Review key={`${selected.id}:${selected.attempt.id}`} client={client} task={selected} matrix={matrix} reviewed={reviewed} onTask={replace} onStarted={started} />
            )}
            <Activity events={events} stream={stream} hasAttempt={Boolean(attemptId)} />
          </>
        ) : importing ? (
          <p className="empty">Importing a repository.</p>
        ) : (
          <div className="start">
            <h2>New task</h2>
            <p className="empty">Import a repository to give a task its own workspace, or add a task to a project here.</p>
            <TaskForm key={`${formProject}:${formOpened}`} matrix={matrix} defaultProject={formProject} busy={pressed === "create"} autoFocus={formOpened > 0} onCreate={(input, chosen) => void run(() => created(input, chosen), undefined, "create")} />
          </div>
        )}
      </main>

      <div className="work-pane">
        <ResizeHandle
          side="w"
          size={{ width: work, height: 0 }}
          min={{ width: workWidth.min, height: 0 }}
          max={{ width: workWidth.max, height: 0 }}
          onResize={(size) => setWork(size.width)}
          label="Resize the work area"
          controls="work-area"
        />
        <WorkArea client={client} task={selected} matrix={matrix} onChanges={setReviewed} />
      </div>

      <footer className="status">
        <span className="status-item">
          <span className="status-key">Branch</span> <code>{selected?.snapshot ? `ada/${selected.id}` : "none"}</code>
        </span>
        <span className="status-item">
          <span className="status-key">Executor</span> local
        </span>
        <span className="status-item">
          <span className="status-key">Agent</span> {used ? `${used.backend}, ${used.model || "default model"}, ${used.effort ? `${used.effort} effort, ` : ""}${used.mode}` : selected?.attempt ? "not recorded" : "none"}
        </span>
        <span className="status-item">
          <span className="status-key">Task</span> {selected ? selected.state : "none selected"}
        </span>
        <button type="button" className="stop" disabled={!cancellable} onClick={() => selected && cancel(selected)}>
          Stop
        </button>
        <span className="status-fill">{tasks.length} tasks</span>
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
        {stopResult && (
          <p role="status">
            {stopResult.reason}: {stopResult.running} running cancellation requested, {stopResult.queued} queued canceled. Waiting for the coordinator to exit.
          </p>
        )}
        <button type="button" className="quit" disabled={stopResult !== null} onClick={quit}>
          Quit Ada and stop work
        </button>
      </footer>
      {importing && (
        <ImportFlow
          client={client}
          matrix={matrix}
          onClose={() => setImporting(false)}
          onProject={(id, name) => setNames((current) => ({ ...current, [id]: name }))}
          onCreated={async (input, chosen) => {
            await created(input, chosen);
            setImporting(false);
          }}
        />
      )}
      <CommandPalette client={client} actions={actions} onSelectTask={select} />
    </div>
  );
}
