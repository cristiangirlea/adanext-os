import type { ChangedFile, Changes as ChangeSet, ProviderChanges } from "../client/api";
import { Marked, PathText } from "../text/Marked";

// The task's own changes (M4.3): the diff from the imported-changes commit to the agent-changes
// commit, file by file. A patch is shown as text, line by line, every hidden character marked;
// nothing in it is ever read as HTML.

const statusNames: Record<string, string> = { M: "modified", A: "added", D: "deleted", R: "renamed", C: "copied", T: "type changed" };

type LineKind = "meta" | "hunk" | "add" | "del" | "ctx";
function lineKind(line: string, inHunk: boolean): LineKind {
  if (line.startsWith("@@")) return "hunk";
  if (!inHunk) return "meta";
  if (line.startsWith("+")) return "add";
  if (line.startsWith("-")) return "del";
  if (line.startsWith("\\")) return "meta";
  return "ctx";
}

export function Patch({ text }: { text: string }) {
  const lines = text.endsWith("\n") ? text.slice(0, -1).split("\n") : text.split("\n");
  let inHunk = false;
  return (
    <div className="patch" role="group" aria-label="Patch">
      {lines.map((line, i) => {
        if (line.startsWith("diff --git ")) inHunk = false;
        const kind = lineKind(line, inHunk);
        if (kind === "hunk") inHunk = true;
        return (
          <div key={i} className={`patch-line patch-${kind}`}>
            <Marked text={line} />
          </div>
        );
      })}
    </div>
  );
}

export function ProviderChangeList({ changes }: { changes: ProviderChanges }) {
  return (
    <ul className="provider-changes">
      {changes.changes.map((c) => (
        <li key={`${c.change}:${c.path}`}>
          <span className="provider-change">{c.change}</span> <PathText path={c.path} />
        </li>
      ))}
      {changes.truncated && <li>and more not listed</li>}
    </ul>
  );
}

function FileChange({ file, open }: { file: ChangedFile; open: boolean }) {
  const mode = file.old_mode !== file.new_mode && file.old_mode && file.new_mode ? `${file.old_mode} to ${file.new_mode}` : "";
  return (
    <details className="file-change" open={open} data-provider={file.provider || undefined}>
      <summary>
        <span className={`change-status change-${file.status}`}>{statusNames[file.status] ?? file.status}</span>
        {file.old_path !== undefined && (
          <>
            <PathText path={file.old_path} raw={file.old_path_raw} /> <span className="to">to</span>{" "}
          </>
        )}
        <PathText path={file.path} raw={file.path_raw} />
        {file.binary ? <span className="tag">binary</span> : <span className="counts">+{file.added} −{file.deleted}</span>}
        {mode && <span className="tag">mode {mode}</span>}
        {file.provider && <span className="tag tag-warn">provider configuration</span>}
      </summary>
      {file.binary ? (
        <p className="empty">A binary file; no patch is shown.</p>
      ) : file.patch === "" ? (
        <p className="empty">No textual change.</p>
      ) : (
        <Patch text={file.patch} />
      )}
      {file.patch_raw !== undefined && <p className="note">This patch is not UTF-8; it is shown with replacement characters.</p>}
      {file.patch_truncated && <p className="note">The patch was cut at 512 KiB.</p>}
    </details>
  );
}

export function ChangesView({ changes }: { changes: ChangeSet }) {
  const open = changes.files.length <= 20;
  return (
    <div className="changes">
      <p className="changes-head">
        <span>
          <code>{changes.base.slice(0, 12)}</code> to <code>{changes.commit.slice(0, 12)}</code>
        </span>
        <span>
          {changes.total_capped ? `at least ${changes.total}` : changes.total} {changes.total === 1 ? "file" : "files"}
        </span>
      </p>
      {changes.stale && <p className="note note-warn">The branch has moved past this commit; read the changes again before accepting.</p>}
      {(changes.files_truncated || changes.bytes_truncated) && (
        <p className="note">
          {changes.files_truncated ? "Only the first 1000 files are listed. " : ""}
          {changes.bytes_truncated ? "Patches stop after 8 MiB in all." : ""}
        </p>
      )}
      {changes.provider_flagged && (
        <div className="note note-warn">
          <p>This attempt changed provider configuration or hooks, which the CLI loads from the worktree.</p>
          {changes.provider_changes ? <ProviderChangeList changes={changes.provider_changes} /> : <p>Ada could not assess what changed.</p>}
        </div>
      )}
      {changes.files.length === 0 ? <p className="empty">The agent changed no files.</p> : changes.files.map((file, i) => <FileChange key={`${i}:${file.path}`} file={file} open={open} />)}
    </div>
  );
}
