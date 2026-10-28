import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { AdaError, type AdaClient, type Task } from "../client/api";
import { effectAttrs } from "../motion/effects";
import { groups, needsTasks, remember, scopes, type Item, type QuickAction, type Scope } from "./items";

export type { QuickAction } from "./items";

// The command palette: Ctrl+P (Cmd+P) opens it on everything, Ctrl+Shift+P on quick actions; the
// browser's print default is prevented either way. A combobox over a listbox, the active option
// named by aria-activedescendant so focus stays in the input. Tasks are read through
// client.listTasks only when a scope that shows them is first opened, and read again on the
// next open.

type Load = { state: "idle" } | { state: "loading" } | { state: "done"; tasks: Task[] } | { state: "failed"; code: string };

interface Props {
  client: AdaClient;
  actions: QuickAction[];
  onSelectTask: (id: string) => void;
}

export function CommandPalette({ client, actions, onSelectTask }: Props) {
  const [open, setOpen] = useState(false);
  const [scope, setScope] = useState<Scope>("all");
  const [query, setQuery] = useState("");
  // A chosen project narrows the tasks to that exact project id until cleared.
  const [project, setProject] = useState<string | null>(null);
  const [active, setActive] = useState(0);
  const [load, setLoad] = useState<Load>({ state: "idle" });
  const [recent, setRecent] = useState<Item[]>([]);
  const dialog = useRef<HTMLDialogElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const returnFocus = useRef<Element | null>(null);
  const generation = useRef(0);
  const id = useId();

  const show = useCallback((initial: Scope) => {
    returnFocus.current = document.activeElement;
    setScope(initial);
    setProject(null);
    setQuery("");
    setActive(0);
    setOpen(true);
  }, []);

  const close = useCallback(() => {
    generation.current++;
    // The modal dialog makes the rest of the page inert: close it before focus goes back.
    const el = dialog.current;
    if (el && typeof el.close === "function") {
      if (el.open) el.close();
    } else {
      el?.removeAttribute("open");
    }
    setOpen(false);
    setLoad({ state: "idle" });
    setProject(null);
    const back = returnFocus.current;
    returnFocus.current = null;
    if (back instanceof HTMLElement && back.isConnected) back.focus();
  }, []);

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (!(e.ctrlKey || e.metaKey) || e.altKey || e.key.toLowerCase() !== "p") return;
      e.preventDefault();
      if (open) close();
      else show(e.shiftKey ? "action" : "all");
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [open, show, close]);

  useLayoutEffect(() => {
    const el = dialog.current;
    if (!open || !el) return;
    if (typeof el.showModal === "function") {
      if (!el.open) el.showModal();
    } else {
      el.setAttribute("open", "");
    }
    input.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open || load.state !== "idle" || !needsTasks(scope)) return;
    const gen = generation.current;
    setLoad({ state: "loading" });
    client.listTasks().then(
      (tasks) => gen === generation.current && setLoad({ state: "done", tasks }),
      (e: unknown) => gen === generation.current && setLoad({ state: "failed", code: e instanceof AdaError ? e.code : "internal" }),
    );
  }, [open, scope, load.state, client]);

  // Keep the active option in view as the keys move it.
  useLayoutEffect(() => {
    if (!open) return;
    const el = document.getElementById(`${id}-option-${active}`);
    el?.scrollIntoView?.({ block: "nearest" });
  }, [open, active, id]);

  if (!open) return null;

  const tasks = load.state === "done" ? load.tasks : null;
  const shown = groups(scope, query, tasks, actions, recent, project);
  const options = shown.flatMap((g) => g.items);
  const current = options.length > 0 ? Math.min(active, options.length - 1) : -1;
  const optionId = (i: number) => `${id}-option-${i}`;
  const listId = `${id}-list`;

  const choose = (item: Item) => {
    setRecent((r) => remember(r, item));
    if (item.kind === "project") {
      setScope("task");
      setProject(item.id);
      setQuery("");
      setActive(0);
      input.current?.focus();
      return;
    }
    close();
    if (item.kind === "task") onSelectTask(item.id);
    else actions.find((a) => a.id === item.id)?.run();
  };

  const onInputKey = (e: KeyboardEvent<HTMLInputElement>) => {
    const last = options.length - 1;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (last < 0) return;
      // From the latest state: keys can repeat faster than renders.
      setActive((a) => {
        const at = Math.min(a, last);
        return e.key === "ArrowDown" ? (at >= last ? 0 : at + 1) : at <= 0 ? last : at - 1;
      });
    } else if (e.ctrlKey && (e.key === "Home" || e.key === "End")) {
      e.preventDefault();
      setActive(e.key === "Home" ? 0 : Math.max(last, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = options[current];
      if (item) choose(item);
    }
  };

  const pick = (s: Scope) => {
    setScope(s);
    setProject(null);
    setActive(0);
    input.current?.focus();
  };

  const loading = needsTasks(scope) && (load.state === "loading" || load.state === "idle");
  const status = loading
    ? "Loading tasks"
    : needsTasks(scope) && load.state === "failed"
      ? `Tasks could not be loaded: ${load.code}`
      : options.length === 0
        ? query === ""
          ? "Nothing to show yet"
          : `No results for "${query}"`
        : options.length === 1
          ? "1 result"
          : `${options.length} results`;

  let index = 0;
  return (
    <dialog
      ref={dialog}
      className="palette"
      aria-label="Command palette"
      {...effectAttrs("entrance")}
      onCancel={(e) => {
        e.preventDefault();
        close();
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.preventDefault();
          close();
        }
      }}
      onClick={(e) => {
        if (e.target === dialog.current) close();
      }}
    >
      <div className="palette-body">
        <input
          ref={input}
          className="palette-input"
          type="text"
          role="combobox"
          aria-label="Search tasks, projects and actions"
          aria-expanded="true"
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={current >= 0 ? optionId(current) : undefined}
          placeholder="Search tasks, projects and actions"
          autoComplete="off"
          spellCheck={false}
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setActive(0);
          }}
          onKeyDown={onInputKey}
        />
        <div className="palette-scopes" role="group" aria-label="Scope">
          {scopes.map((s) => (
            <button key={s.scope} type="button" className="chip" aria-pressed={scope === s.scope} onClick={() => pick(s.scope)}>
              {s.label}
            </button>
          ))}
        </div>
        {project !== null && (
          <p className="palette-filter">
            Tasks in <code>{project}</code>{" "}
            <button
              type="button"
              className="chip"
              aria-label="Show all tasks"
              onClick={() => {
                setProject(null);
                setActive(0);
                input.current?.focus();
              }}
            >
              Show all
            </button>
          </p>
        )}
        <div className="palette-list" role="listbox" id={listId} aria-label="Results" aria-busy={loading}>
          {shown.map((group, g) => (
            <div key={group.name} role="group" aria-labelledby={`${id}-group-${g}`} className="palette-group">
              <div id={`${id}-group-${g}`} role="presentation" className="palette-group-name">
                {group.name}
              </div>
              {group.items.map((item) => {
                const i = index++;
                return (
                  <div
                    key={`${group.name}:${item.kind}:${item.id}`}
                    id={optionId(i)}
                    role="option"
                    aria-selected={i === current}
                    className="palette-option"
                    data-kind={item.kind}
                    onMouseDown={(e) => e.preventDefault()}
                    onMouseMove={() => i !== current && setActive(i)}
                    onClick={() => choose(item)}
                  >
                    <span className="palette-label">{item.label}</span>
                    {item.detail && <span className="palette-detail">{item.detail}</span>}
                  </div>
                );
              })}
            </div>
          ))}
        </div>
        <p className="palette-status" role="status" aria-live="polite" data-testid="palette-status">
          {status}
        </p>
        <footer className="palette-hints" data-testid="palette-hints">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> move
          </span>
          <span>
            <kbd>Enter</kbd> choose
          </span>
          <span>
            <kbd>Esc</kbd> close
          </span>
          <span>
            <kbd>Ctrl</kbd> <kbd>Shift</kbd> <kbd>P</kbd> actions
          </span>
        </footer>
      </div>
    </dialog>
  );
}
