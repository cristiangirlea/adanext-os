import { useEffect, useState } from "react";
import type { AdaClient, DirectoryListing, ImportRoot, Project, RootInspection } from "../client/api";
import { PathText } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "./errors";
import { BlockerList } from "./ImportFlow";
import { RootDialog } from "./RootDialog";

export function RootChooser({ client, onImport, onClose }: { client: AdaClient; onImport: (project: Project) => void; onClose: () => void }) {
  const [roots, setRoots] = useState<ImportRoot[]>([]);
  const [root, setRoot] = useState<ImportRoot | null>(null);
  const [path, setPath] = useState("");
  const [listing, setListing] = useState<DirectoryListing | null>(null);
  const [inspection, setInspection] = useState<RootInspection | null>(null);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState<Shown | null>(null);
  useEffect(() => {
    let live = true;
    client.importRoots().then((v) => { if (live) setRoots(v); }, (e) => { if (live) setError(shown(e)); })
      .finally(() => { if (live) setBusy(false); });
    return () => { live = false; };
  }, [client]);

  const act = async (fn: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try { await fn(); } catch (e) { setError(shown(e)); } finally { setBusy(false); }
  };
  const browse = (next: ImportRoot, rel: string) => act(async () => {
    const children = await client.browseRoot(next.id, rel);
    setRoot(next);
    setPath(rel);
    setListing(children);
    setInspection(null);
    setName(rel.split("/").pop() || next.label);
  });
  const parts = path ? path.split("/") : [];
  return (
    <RootDialog title="Import a repository" onClose={onClose}>
      {error && <ErrorLine error={error} />}
      <fieldset disabled={busy} aria-label="Choose a repository">
        {!root ? <>
          <legend>Granted folders</legend>
          {!busy && !roots.length && <p>No import folders have been granted to your account.</p>}
          {roots.map((r) => <button key={r.id} onClick={() => void browse(r, "")}>{r.label}</button>)}
        </> : <>
          <legend>Choose a folder</legend>
          <nav aria-label="Folder breadcrumbs">
            <button onClick={() => { setRoot(null); setInspection(null); }}>All roots</button>
            <button onClick={() => void browse(root, "")}>{root.label}</button>
            {parts.map((part, index) => <button key={index} onClick={() => void browse(root, parts.slice(0, index + 1).join("/"))}>{part}</button>)}
          </nav>
          {listing?.directories.map((dir) => <button key={dir.path} onClick={() => void browse(root, dir.path)}><PathText path={dir.name} /></button>)}
          {listing?.truncated && <p>Only the first 1,000 directories are shown.</p>}
          <button onClick={() => void act(async () => { setInspection(await client.inspectInRoot(root.id, path)); })}>Inspect</button>
          {inspection && <>
            <BlockerList blockers={inspection.blockers} />
            {inspection.findings.map((f) => <section key={f.code} aria-label="Provider configuration">
              <h3>Provider configuration</h3>
              <p>The agent can load these files when it runs in the worktree.</p>
              <ul>{f.paths.map(p => <li key={p}><PathText path={p} /></li>)}</ul>
              {f.count > f.paths.length && <p>and {f.count - f.paths.length} more</p>}
            </section>)}
            <h3>Untracked files</h3>
            {inspection.untracked.length ? <ul>{inspection.untracked.map(p => <li key={p}><PathText path={p} /></li>)}</ul> : <p>No untracked files.</p>}
            {!inspection.blockers.length && <>
              <label>Project name<input maxLength={200} value={name} onChange={e => setName(e.target.value)} /></label>
              <button onClick={() => void act(async () => onImport(await client.importFromRoot(root.id, path, name.trim() || undefined)))}>Import</button>
            </>}
          </>}
        </>}
      </fieldset>
    </RootDialog>
  );
}
