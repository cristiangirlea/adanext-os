import { useState, type FormEvent } from "react";
import type { AdaClient, SignedInAccount } from "../client/api";
import { BusyButton } from "../motion/parts";
import { ErrorLine, shown, type Shown } from "../workspace/errors";

// Sign-in to Ada from a browser: a passkey first; a password where the owner allowed one for the
// account, which adad refuses otherwise; and the way to enroll with a code from the owner.

export function SignIn({ client, onSignedIn, onEnroll }: { client: AdaClient; onSignedIn: (account: SignedInAccount) => void; onEnroll: () => void }) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<Shown | null>(null);
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");

  const attempt = async (what: string, fn: () => Promise<SignedInAccount>) => {
    setBusy(what);
    setError(null);
    try {
      onSignedIn(await fn());
    } catch (e) {
      setError(shown(e));
      setBusy("");
    }
  };
  const withPassword = (e: FormEvent) => {
    e.preventDefault();
    if (name.trim() === "" || password === "") return;
    void attempt("password", () => client.passwordLogin(name.trim(), password));
  };

  return (
    <main className="sign-in" aria-labelledby="sign-in-title">
      <span className="wordmark">Ada</span>
      <h1 id="sign-in-title">Sign in</h1>
      {error && <ErrorLine error={error} />}
      <section aria-labelledby="sign-in-passkey">
        <h2 id="sign-in-passkey">With a passkey</h2>
        <BusyButton busy={busy === "passkey"} disabled={busy !== ""} onClick={() => void attempt("passkey", () => client.passkeyLogin())}>
          Sign in with a passkey
        </BusyButton>
      </section>
      <form className="task-form" onSubmit={withPassword} aria-labelledby="sign-in-password">
        <h2 id="sign-in-password">With a password</h2>
        <p className="hint">Only where the owner allowed a password for your account.</p>
        <label>
          Account name
          <input value={name} autoComplete="username" maxLength={256} onChange={(e) => setName(e.target.value)} />
        </label>
        <label>
          Password
          <input type="password" value={password} autoComplete="current-password" onChange={(e) => setPassword(e.target.value)} />
        </label>
        <div>
          <BusyButton type="submit" busy={busy === "password"} disabled={busy !== "" || name.trim() === "" || password === ""}>
            Sign in with the password
          </BusyButton>
        </div>
      </form>
      <p>
        New here, or given a code by the owner?{" "}
        <button type="button" className="link" disabled={busy !== ""} onClick={onEnroll}>
          Enroll with a code
        </button>
      </p>
    </main>
  );
}
