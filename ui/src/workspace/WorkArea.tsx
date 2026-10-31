import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { AdaError, type AdaClient, type Capabilities, type Changes, type CheckResult, type Task } from "../client/api";
import { Skeleton } from "../motion/parts";
import { ChangesView } from "./Changes";
import { ChecksView } from "./Checks";
import { ErrorLine, shown, type Shown } from "./errors";
import { FilesView } from "./Files";
import { ExperienceCard } from "./ExperienceCard";

// The work area beside the task: Changes, Checks and Files of the selected task, each read from
// adad when its tab is shown and read again when the task moves on. Preview waits for M5.

const tabs = [
  { id: "changes", name: "Changes" },
  { id: "checks", name: "Checks" },
  { id: "files", name: "Files" },
] as const;
type Tab = (typeof tabs)[number]["id"];

type Loaded<T> = { state: "loading" } | { state: "none"; why: string } | { state: "error"; error: Shown } | { state: "ok"; value: T };

// A read that answers no_changes is "nothing yet", not an error.
function useRead<T>(read: (() => Promise<T>) | null, key: string): Loaded<T> {
  const [loaded, setLoaded] = useState<Loaded<T>>({ state: "loading" });
  const readRef = useRef(read);
  readRef.current = read;
  useEffect(() => {
    const fn = readRef.current;
    if (!fn) {
      setLoaded({ state: "none", why: "" });
      return;
    }
    let live = true;
    setLoaded({ state: "loading" });
    fn().then(
      (value) => live && setLoaded({ state: "ok", value }),
      (e: unknown) => {
        if (!live) return;
        if (e instanceof AdaError && e.code === "no_changes") setLoaded({ state: "none", why: e.message });
        else setLoaded({ state: "error", error: shown(e) });
      },
    );
    return () => {
      live = false;
    };
  }, [key]);
  return loaded;
}

export function WorkArea({ client, task, matrix, onChanges }: { client: AdaClient; task: Task | null; matrix: Capabilities | null; onChanges?: (changes: Changes | null) => void }) {
  const [tab, setTab] = useState<Tab>("changes");
  const buttons = useRef<Record<string, HTMLButtonElement | null>>({});
  const commit = task?.attempt?.tested_commit ?? "";
  const key = task ? `${task.id}:${task.revision}:${commit}` : "";
  const changes = useRead<Changes>(task && tab === "changes" && commit ? () => client.changes(task.id) : null, `${tab}:${key}`);
  const checks = useRead<CheckResult[]>(task && tab === "checks" ? () => client.checks(task.id) : null, `${tab}:${key}`);
  // The review binds the changes the reviewer last read here (their commit and generation).
  const shownChanges = changes.state === "ok" ? changes.value : null;
  useEffect(() => {
    if (tab === "changes") onChanges?.(shownChanges);
  }, [tab, shownChanges, onChanges]);

  const onKey = (e: KeyboardEvent) => {
    const at = tabs.findIndex((t) => t.id === tab);
    let next = at;
    if (e.key === "ArrowRight") next = (at + 1) % tabs.length;
    else if (e.key === "ArrowLeft") next = (at + tabs.length - 1) % tabs.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = tabs.length - 1;
    else return;
    e.preventDefault();
    const id = tabs[next]!.id;
    setTab(id);
    buttons.current[id]?.focus();
  };

  const body = () => {
    if (!task) return <p className="empty">Select a task to see its changes, checks and files.</p>;
    switch (tab) {
      case "changes":
        if (!commit) return <p className="empty">No agent changes yet: they appear once an attempt ends and Ada commits them.</p>;
        if (changes.state === "loading") return <Skeleton rows={3} label="Loading changes" />;
        if (changes.state === "error") return <ErrorLine error={changes.error} alert={false} />;
        if (changes.state === "none") return <p className="empty">No agent changes yet.</p>;
        return <ChangesView changes={changes.value} />;
      case "checks":
        if (checks.state === "loading") return <Skeleton rows={2} label="Loading checks" />;
        if (checks.state === "error") return <ErrorLine error={checks.error} alert={false} />;
        return <ChecksView client={client} taskId={task.id} results={checks.state === "ok" ? checks.value : []} matrix={matrix} />;
      case "files":
        if (!commit) return <p className="empty">No agent-changes commit yet.</p>;
        return <FilesView client={client} taskId={task.id} commit={commit} />;
    }
  };

  return (
    <section className="work-area" aria-label="Work area">
      <ExperienceCard client={client} />
      <div className="tabs" role="tablist" aria-label="Work area" onKeyDown={onKey}>
        {tabs.map((t) => (
          <button
            key={t.id}
            ref={(el) => {
              buttons.current[t.id] = el;
            }}
            type="button"
            role="tab"
            id={`tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls="work-panel"
            tabIndex={tab === t.id ? 0 : -1}
            className="tab"
            onClick={() => setTab(t.id)}
          >
            {t.name}
            {t.id === "checks" && <span className="tab-mark">unsandboxed</span>}
          </button>
        ))}
      </div>
      <div className="work-panel" id="work-panel" role="tabpanel" aria-labelledby={`tab-${tab}`} tabIndex={0}>
        {body()}
      </div>
    </section>
  );
}
