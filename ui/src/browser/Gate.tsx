import { useCallback, useEffect, useRef, useState } from "react";
import type { AdaClient, SignedInAccount } from "../client/api";
import { App } from "../App";
import { Skeleton } from "../motion/parts";
import { ErrorLine, shown, type Shown } from "../workspace/errors";
import { Enroll } from "./Enroll";
import { signedOut } from "./session";
import { reloadToStart } from "./reload";
import { SignIn } from "./SignIn";

// The browser shell: the account first, then the workspace. Without a session (401) it shows the
// sign-in page, or the enrollment page a person reaches from it with their code. The desktop
// shell never comes here: its channel is the owner's own and needs no sign-in. A session lost
// while signed in (any 401), or another account found when the tab comes back into view (the
// cookie is shared with other tabs), reloads the page: that ends every request, stream, timer and
// dialog at once, so no late answer can act on a newer session. Leave, and the account page's
// actions that end this session, end the same way. The workspace is unmounted first, so none of
// its controls can send anything while the new page loads.

type State = { kind: "retired" } | { kind: "loading" } | { kind: "failed"; error: Shown } | { kind: "signed-out"; page: "sign-in" | "enroll" } | { kind: "signed-in"; account: SignedInAccount };

export function Root({ client }: { client: AdaClient }) {
  return client.channel === "desktop" ? <App client={client} /> : <Gate client={client} />;
}

export function Gate({ client }: { client: AdaClient }) {
  const [state, setState] = useState<State>({ kind: "loading" });

  const load = useCallback(() => {
    setState({ kind: "loading" });
    client.account().then(
      (account) => setState({ kind: "signed-in", account }),
      (e: unknown) => setState(signedOut(e) ? { kind: "signed-out", page: "sign-in" } : { kind: "failed", error: shown(e) }),
    );
  }, [client]);
  useEffect(load, [load]);

  // The account this page was signed in as when its workspace mounted; null while signed out.
  const current = useRef<string | null>(null);
  current.current = state.kind === "signed-in" ? state.account.id : null;
  const reloading = useRef(false);
  const reload = useCallback(() => {
    if (reloading.current) return;
    reloading.current = true;
    current.current = null;
    setState({ kind: "retired" });
    reloadToStart();
  }, []);

  // Sign-in's own reads answer 401 too; only a signed-in page reloads.
  useEffect(() => client.onSessionEnded(() => current.current !== null && reload()), [client, reload]);

  useEffect(() => {
    let mounted = true;
    let checking = false;
    const recheck = () => {
      const held = current.current;
      if (held === null || checking || document.visibilityState === "hidden") return;
      checking = true;
      client.account().then(
        (account) => {
          checking = false;
          if (mounted && current.current === held && account.id !== held) reload();
        },
        (e: unknown) => {
          checking = false;
          if (mounted && current.current === held && signedOut(e)) reload();
        },
      );
    };
    window.addEventListener("focus", recheck);
    document.addEventListener("visibilitychange", recheck);
    return () => {
      mounted = false;
      window.removeEventListener("focus", recheck);
      document.removeEventListener("visibilitychange", recheck);
    };
  }, [client, reload]);

  const signedIn = (account: SignedInAccount) => setState({ kind: "signed-in", account });

  switch (state.kind) {
    case "retired":
      return (
        <main className="sign-in" aria-label="Ada">
          <p role="status">Signing in again…</p>
        </main>
      );
    case "loading":
      return (
        <main className="sign-in" aria-label="Ada">
          <Skeleton rows={2} label="Checking your session" />
        </main>
      );
    case "failed":
      return (
        <main className="sign-in" aria-label="Ada">
          <ErrorLine error={state.error} />
          <button type="button" onClick={load}>
            Try again
          </button>
        </main>
      );
    case "signed-out":
      return state.page === "sign-in" ? (
        <SignIn client={client} onSignedIn={signedIn} onEnroll={() => setState({ kind: "signed-out", page: "enroll" })} />
      ) : (
        <Enroll client={client} onSignedIn={signedIn} onBack={() => setState({ kind: "signed-out", page: "sign-in" })} />
      );
    case "signed-in":
      return <App client={client} account={state.account} onLeave={reload} />;
  }
}
