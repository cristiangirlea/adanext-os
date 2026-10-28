import { useId, useState, type FormEvent, type Ref } from "react";
import type { Capabilities, CheckDefinition, ExecutionPolicy, NewTask } from "../client/api";
import { BusyButton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { parseArgv } from "./argv";
import { defaultPolicy } from "./policy";
import { PolicyPicker } from "./PolicyPicker";

interface CheckRow {
  key: number;
  name: string;
  command: string;
  workdir: string;
  timeout: string;
}

// The plain statement M4.2 asks for wherever checks are declared or shown.
export function UnsandboxedNotice({ matrix }: { matrix: Capabilities | null }) {
  return (
    <p className="unsandboxed">
      <strong>Checks are not sandboxed.</strong> Each runs the repository's code and the agent's changes with your rights, on this computer, outside any backend sandbox and with network.
      {matrix?.checks.detail ? ` ${matrix.checks.detail}` : ""}
    </p>
  );
}

function toChecks(rows: CheckRow[]): CheckDefinition[] | string {
  const out: CheckDefinition[] = [];
  for (const row of rows) {
    if (row.command.trim() === "") continue;
    const argv = parseArgv(row.command);
    if (!Array.isArray(argv)) return `${row.name || "a check"}: ${argv.error}`;
    const check: CheckDefinition = { argv };
    if (row.name.trim()) check.name = row.name.trim();
    if (row.workdir.trim()) check.workdir = row.workdir.trim();
    if (row.timeout.trim()) check.timeout = row.timeout.trim();
    out.push(check);
  }
  return out;
}

// A new task: objective, acceptance criteria, declared checks, and the agent to run it with.
// project fixes the project (the import flow, or a project chosen in the navigation); without
// it the form asks for one.
export function TaskForm({
  matrix,
  project,
  defaultProject = "",
  snapshot,
  busy,
  onCreate,
  objectiveRef,
}: {
  matrix: Capabilities | null;
  project?: { id: string; name: string };
  defaultProject?: string;
  snapshot?: string;
  busy: boolean;
  onCreate: (input: NewTask, policy: ExecutionPolicy) => void;
  objectiveRef?: Ref<HTMLInputElement>;
}) {
  const id = useId();
  const [projectId, setProjectId] = useState(project?.id ?? defaultProject);
  const [objective, setObjective] = useState("");
  const [criteria, setCriteria] = useState("");
  const [rows, setRows] = useState<CheckRow[]>([]);
  const [next, setNext] = useState(1);
  const [chosen, setPolicy] = useState<ExecutionPolicy | null>(null);
  const [problem, setProblem] = useState("");
  const policy = chosen ?? defaultPolicy(matrix);

  const update = (key: number, field: keyof Omit<CheckRow, "key">, value: string) => setRows((current) => current.map((r) => (r.key === key ? { ...r, [field]: value } : r)));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const checks = toChecks(rows);
    if (typeof checks === "string") {
      setProblem(checks);
      return;
    }
    setProblem("");
    const input: NewTask = { project: project?.id ?? projectId, objective };
    if (criteria.trim()) input.criteria = criteria;
    if (snapshot) input.snapshot = snapshot;
    if (checks.length) input.checks = checks;
    // The fields stay: a created task replaces the form, and a refused one keeps what was typed.
    onCreate(input, policy);
  };

  return (
    <form className="task-form" onSubmit={submit} aria-label="New task">
      {project ? (
        <p className="task-form-project">
          In <strong>{project.name}</strong>
          {snapshot && <> from snapshot <code>{snapshot.slice(0, 12)}</code></>}
        </p>
      ) : (
        <label>
          Project
          <input value={projectId} onChange={(e) => setProjectId(e.target.value)} required />
        </label>
      )}
      <label>
        Objective
        <input ref={objectiveRef} value={objective} onChange={(e) => setObjective(e.target.value)} required />
      </label>
      <label>
        Acceptance criteria
        <textarea rows={2} value={criteria} onChange={(e) => setCriteria(e.target.value)} />
      </label>
      <fieldset className="checks-editor">
        <legend>Checks</legend>
        <UnsandboxedNotice matrix={matrix} />
        {rows.map((row, n) => {
          const argv = parseArgv(row.command);
          return (
            <div key={row.key} className="check-row" role="group" aria-label={`Check ${n + 1}`}>
              <label>
                Name
                <input value={row.name} onChange={(e) => update(row.key, "name", e.target.value)} />
              </label>
              <label className="check-command">
                Command
                <input className="mono" value={row.command} onChange={(e) => update(row.key, "command", e.target.value)} aria-describedby={`${id}-argv-${row.key}`} />
              </label>
              <label>
                Directory in the worktree
                <input className="mono" value={row.workdir} placeholder="." onChange={(e) => update(row.key, "workdir", e.target.value)} />
              </label>
              <label>
                Timeout
                <input className="mono" value={row.timeout} placeholder="10m" onChange={(e) => update(row.key, "timeout", e.target.value)} />
              </label>
              <button type="button" className="quiet" onClick={() => setRows((current) => current.filter((r) => r.key !== row.key))}>
                Remove
              </button>
              <p className="argv" id={`${id}-argv-${row.key}`}>
                {Array.isArray(argv) ? (
                  argv.length ? (
                    <>
                      Runs, without a shell:{" "}
                      {argv.map((word, i) => (
                        <code key={i} className="argv-word">
                          <Marked text={word} />
                        </code>
                      ))}
                    </>
                  ) : (
                    "Type the command; it runs without a shell."
                  )
                ) : (
                  argv.error
                )}
              </p>
            </div>
          );
        })}
        <button
          type="button"
          className="quiet"
          disabled={rows.length >= 16}
          onClick={() => {
            setRows((current) => [...current, { key: next, name: "", command: "", workdir: "", timeout: "" }]);
            setNext(next + 1);
          }}
        >
          Add a check
        </button>
      </fieldset>
      <PolicyPicker matrix={matrix} value={policy} onChange={setPolicy} />
      {problem && <p className="error">{problem}</p>}
      <BusyButton type="submit" busy={busy}>
        Create
      </BusyButton>
    </form>
  );
}
