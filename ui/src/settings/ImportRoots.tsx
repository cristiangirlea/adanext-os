import { useEffect, useState } from "react";
import type { Account, AdaClient, ManagedImportRoot } from "../client/api";
import { ErrorLine, shown, type Shown } from "../workspace/errors";
import { RootDialog } from "../workspace/RootDialog";

export function ImportRoots({ client, onClose }: { client: AdaClient; onClose: () => void }) {
  const [roots, setRoots] = useState<ManagedImportRoot[]>([]);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState<Shown | null>(null);
  useEffect(() => {
    let live = true;
    Promise.all([client.allImportRoots(), client.listAccounts()]).then(([r, a]) => {
      if (live) { setRoots(r); setAccounts(a); }
    }, e => { if (live) setError(shown(e)); }).finally(() => { if (live) setBusy(false); });
    return () => { live = false; };
  }, [client]);
  const change = async (action: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await action();
      setRoots(await client.allImportRoots());
    } catch (e) { setError(shown(e)); } finally { setBusy(false); }
  };
  return (
    <RootDialog title="Import roots" onClose={onClose}>
      {error && <ErrorLine error={error} />}
      <fieldset disabled={busy}>
        <legend>Add an import root</legend>
        <label>Folder label<input maxLength={200} placeholder="Folder name by default" value={label} onChange={e => setLabel(e.target.value)} /></label>
        <button onClick={() => void change(async () => { if (await client.addImportRoot(label.trim() || undefined)) setLabel(""); })}>Add folder</button>
      </fieldset>
      {roots.map(root => <fieldset key={root.id} disabled={busy}>
        <legend>{root.label}</legend>
        {accounts.map(account => <label className="inline" key={account.id}>
          <input type="checkbox" checked={root.grants.includes(account.id)} onChange={e => {
            const grant = e.target.checked;
            void change(() => grant ? client.grantImportRoot(root.id, account.id) : client.revokeImportRoot(root.id, account.id));
          }} />{account.name}
        </label>)}
        <button onClick={() => void change(() => client.removeImportRoot(root.id))} aria-label={`Remove ${root.label}`}>Remove</button>
      </fieldset>)}
    </RootDialog>
  );
}
