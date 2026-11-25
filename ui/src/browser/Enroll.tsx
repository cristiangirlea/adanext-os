import { useState, type FormEvent } from "react";
import type { AdaClient, EnrollmentStart, SignedInAccount } from "../client/api";
import { BusyButton } from "../motion/parts";
import { Marked } from "../text/Marked";
import { ErrorLine, shown, type Shown } from "../workspace/errors";
import { maxPasswordBytes, minPasswordCodePoints, validPassword } from "./password";

// Enrollment with the single-use code the owner gave: the code first, then a passkey, or a
// password where the code's account allows one. Either signs the browser in. The code is kept
// with the options it was checked for, and the field stays shut until "Use another code".


export function Enroll({ client, onSignedIn, onBack }: { client: AdaClient; onSignedIn: (account: SignedInAccount) => void; onBack: () => void }) {
  const [code, setCode] = useState("");
  const [accepted, setAccepted] = useState<{ code: string; start: EnrollmentStart } | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<Shown | null>(null);
  const [passkeyName, setPasskeyName] = useState("Passkey");
  const [password, setPassword] = useState("");

  const act = async (what: string, fn: () => Promise<void>) => {
    setBusy(what);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(shown(e));
    } finally {
      setBusy("");
    }
  };
  const check = (e: FormEvent) => {
    e.preventDefault();
    const submitted = code.trim();
    if (submitted === "" || busy !== "") return;
    void act("code", async () => setAccepted({ code: submitted, start: await client.enrollmentOptions(submitted) }));
  };
  // A passkey ceremony is used once: a refused one is begun again from the same code.
  const passkey = () => {
    if (!accepted) return;
    const { code: submitted, start } = accepted;
    void act("passkey", async () => {
      try {
        onSignedIn(await client.enrollPasskey(submitted, start, passkeyName.trim()));
      } catch (e) {
        const again = await client.enrollmentOptions(submitted).catch(() => start);
        setAccepted((held) => (held?.code === submitted ? { code: submitted, start: again } : held));
        throw e;
      }
    });
  };
  const withPassword = (e: FormEvent) => {
    e.preventDefault();
    if (!accepted || !validPassword(password)) return;
    const submitted = accepted.code;
    void act("password", async () => onSignedIn(await client.enrollPassword(submitted, password)));
  };
  const start = accepted?.start ?? null;
  // Another code starts over: nothing typed for the last one carries to the next.
  const anotherCode = () => {
    setAccepted(null);
    setPassword("");
    setPasskeyName("Passkey");
    setError(null);
  };

  return (
    <main className="sign-in" aria-labelledby="enroll-title">
      <span className="wordmark">Ada</span>
      <h1 id="enroll-title">Enroll with a code</h1>
      {error && <ErrorLine error={error} />}
      <form className="task-form" onSubmit={check} aria-label="Your code">
        <label>
          Enrollment code
          <input value={code} autoComplete="one-time-code" maxLength={64} disabled={busy !== "" || accepted !== null} onChange={(e) => setCode(e.target.value)} />
        </label>
        <div>
          {accepted === null ? (
            <BusyButton type="submit" busy={busy === "code"} disabled={busy !== "" || code.trim() === ""}>
              Continue
            </BusyButton>
          ) : (
            <button type="button" className="quiet" disabled={busy !== ""} onClick={anotherCode}>
              Use another code
            </button>
          )}
        </div>
      </form>
      {start && (
        <>
          <p>
            This code is for the account <Marked text={start.account.name} />. It works once.
          </p>
          <section className="task-form" aria-labelledby="enroll-passkey">
            <h2 id="enroll-passkey">Add a passkey</h2>
            <label>
              Name this passkey
              <input value={passkeyName} maxLength={64} onChange={(e) => setPasskeyName(e.target.value)} />
            </label>
            <div>
              <BusyButton busy={busy === "passkey"} disabled={busy !== "" || passkeyName.trim() === ""} onClick={passkey}>
                Add a passkey and sign in
              </BusyButton>
            </div>
          </section>
          {start.account.password_allowed && (
            <form className="task-form" onSubmit={withPassword} aria-labelledby="enroll-password">
              <h2 id="enroll-password">Or set a password</h2>
              <label>
                New password
                <input type="password" value={password} autoComplete="new-password" onChange={(e) => setPassword(e.target.value)} />
              </label>
              <p className="hint">At least {minPasswordCodePoints} characters, and at most {maxPasswordBytes} bytes.</p>
              <div>
                <BusyButton type="submit" busy={busy === "password"} disabled={busy !== "" || !validPassword(password)}>
                  Set the password and sign in
                </BusyButton>
              </div>
            </form>
          )}
        </>
      )}
      <p>
        <button type="button" className="link" disabled={busy !== ""} onClick={onBack}>
          Back to sign-in
        </button>
      </p>
    </main>
  );
}
