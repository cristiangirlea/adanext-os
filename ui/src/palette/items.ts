import type { Task } from "../client/api";

// What the palette lists: tasks and projects from AdaClient.listTasks (a project is the distinct
// project id of those tasks), and quick actions the app defines locally.
export type Scope = "all" | "project" | "task" | "action";
export type Kind = Exclude<Scope, "all">;

export const scopes: { scope: Scope; label: string }[] = [
  { scope: "all", label: "All" },
  { scope: "project", label: "Projects" },
  { scope: "task", label: "Tasks" },
  { scope: "action", label: "Actions" },
];

export interface QuickAction {
  id: string;
  label: string;
  run: () => void;
}

export interface Item {
  kind: Kind;
  id: string;
  label: string;
  detail?: string;
}

export interface Group {
  name: string;
  items: Item[];
}

export const needsTasks = (scope: Scope) => scope !== "action";

function matches(item: Item, query: string): boolean {
  if (query === "") return true;
  const q = query.toLowerCase();
  return item.label.toLowerCase().includes(q) || (item.detail?.toLowerCase().includes(q) ?? false);
}

export function taskItems(tasks: Task[]): Item[] {
  return tasks.map((t) => ({ kind: "task", id: t.id, label: t.objective, detail: t.project }));
}

export function projectItems(tasks: Task[]): Item[] {
  return [...new Set(tasks.map((t) => t.project))].map((p) => ({ kind: "project", id: p, label: p }));
}

// groups builds what one scope shows for a query: Recent first (only before anything is typed),
// then quick actions, tasks and projects as the scope allows.
export function groups(scope: Scope, query: string, tasks: Task[] | null, actions: QuickAction[], recent: Item[]): Group[] {
  const shows = (kind: Kind) => scope === "all" || scope === kind;
  const actionList: Item[] = actions.map((a) => ({ kind: "action", id: a.id, label: a.label }));
  const current = (item: Item): Item | undefined => (item.kind === "action" ? actionList.find((a) => a.id === item.id) : item);
  const out: Group[] = [];
  if (query === "") {
    const items = recent.filter((r) => shows(r.kind)).map(current).filter((r): r is Item => r !== undefined);
    if (items.length > 0) out.push({ name: "Recent", items });
  }
  if (shows("action")) out.push({ name: "Quick actions", items: actionList.filter((i) => matches(i, query)) });
  if (shows("task") && tasks) out.push({ name: "Tasks", items: taskItems(tasks).filter((i) => matches(i, query)) });
  if (shows("project") && tasks) out.push({ name: "Projects", items: projectItems(tasks).filter((i) => matches(i, query)) });
  return out.filter((g) => g.items.length > 0);
}

export function remember(recent: Item[], item: Item, limit = 5): Item[] {
  return [item, ...recent.filter((r) => r.kind !== item.kind || r.id !== item.id)].slice(0, limit);
}
