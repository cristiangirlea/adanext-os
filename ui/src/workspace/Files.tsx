import { useEffect, useState } from "react";
import type { AdaClient, FileView } from "../client/api";
import { Marked, PathText } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "./errors";

// A read-only tree of the agent-changes commit (M4.3's files route). Directories are opened one
// at a time from adad; a symlink shows its stored target and is never followed; a file shows as
// text. Paths come from git and are marked like every other path.

export function FilesView({ client, taskId, commit }: { client: AdaClient; taskId: string; commit: string }) {
  const [path, setPath] = useState("");
  const [view, setView] = useState<FileView | null>(null);
  const [error, setError] = useState<Shown | null>(null);

  useEffect(() => {
    let live = true;
    setError(null);
    client.files(taskId, path).then(
      (v) => live && setView(v),
      (e: unknown) => live && setError(shown(e)),
    );
    return () => {
      live = false;
    };
  }, [client, taskId, path, commit]);

  const parts = path === "" ? [] : path.split("/");
  return (
    <div className="files">
      <nav className="crumbs" aria-label="Folder">
        <button type="button" className="link" onClick={() => setPath("")} aria-current={path === "" ? "location" : undefined}>
          root
        </button>
        {parts.map((part, i) => (
          <span key={i}>
            {" / "}
            <button type="button" className="link" onClick={() => setPath(parts.slice(0, i + 1).join("/"))} aria-current={i === parts.length - 1 ? "location" : undefined}>
              <Marked text={part} />
            </button>
          </span>
        ))}
      </nav>
      {error && <ErrorLine error={error} alert={false} />}
      {view && view.path === path && <Entry view={view} open={setPath} />}
    </div>
  );
}

function Entry({ view, open }: { view: FileView; open: (path: string) => void }) {
  switch (view.type) {
    case "tree":
      return (
        <>
          <table className="tree">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Kind</th>
                <th scope="col">Size</th>
              </tr>
            </thead>
            <tbody>
              {(view.entries ?? []).map((e) => (
                <tr key={e.path} className="tree-entry">
                  <td>
                    {e.path_raw === undefined && (e.type === "tree" || e.type === "blob" || e.type === "symlink") ? (
                      <button type="button" className="link" onClick={() => open(e.path)}>
                        <PathText path={e.name} />
                        {e.type === "tree" ? "/" : ""}
                      </button>
                    ) : (
                      <PathText path={e.name} raw={e.name_raw} />
                    )}
                  </td>
                  <td>{e.type === "tree" ? "folder" : e.type === "symlink" ? "link" : e.type === "commit" ? "submodule" : "file"}</td>
                  <td>{e.type === "blob" ? `${e.size} B` : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {view.entries_truncated && <p className="note">Only the first 5000 entries are listed.</p>}
        </>
      );
    case "symlink":
      return (
        <p className="symlink">
          A link to <PathText path={view.target ?? ""} raw={view.target_raw} />. Ada shows where it points and never follows it.
        </p>
      );
    case "commit":
      return (
        <p>
          A submodule at <code>{view.submodule}</code>.
        </p>
      );
    default:
      return (
        <>
          {view.encoding === "base64" ? (
            <p className="empty">Not text ({view.size} bytes); not shown.</p>
          ) : (
            <pre className="file-content" aria-label={`Content of ${view.path}`}>
              <Marked text={view.content ?? ""} keepNewlines />
            </pre>
          )}
          {view.truncated && <p className="note">Only the first 1 MiB is shown.</p>}
        </>
      );
  }
}
