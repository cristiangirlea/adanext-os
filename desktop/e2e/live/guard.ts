// Guards for the live demonstration, kept free of Playwright so Vitest can test them.

// The opt-in: exactly "1", as cmd/ada-live reads it.
export function liveEnabled(env: Record<string, string | undefined>): boolean {
  return env.ADANEXT_LIVE === "1";
}

const emailShaped = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;

// One slash form, one case, no trailing slash: how paths are compared.
function normal(p: string): string {
  return p.replaceAll("\\", "/").toLowerCase().replace(/\/+$/, "");
}

// personalText names what must not be on screen in a screenshot: e-mail-shaped text, the home
// directory and the user's name. It says what kind it found, never the text itself.
export function personalText(text: string, user = "", home = ""): string[] {
  const found: string[] = [];
  const emails = text.match(emailShaped)?.length ?? 0;
  if (emails) found.push(`${emails} e-mail-shaped text`);
  const lower = normal(text);
  if (home && lower.includes(normal(home))) found.push("the home directory");
  if (user.length >= 3 && lower.includes(user.toLowerCase())) found.push("the user name");
  return found;
}

interface Scanned {
  tagName: string;
  value?: string;
  selectedOptions?: ArrayLike<{ text: string }>;
  getAttribute(name: string): string | null;
}

// screenText gathers what the page can show: its text, every form control's value (and a
// select's chosen option), and title, placeholder and aria-label attributes. It runs in the page
// through evaluate, so it uses nothing outside itself.
export function screenText(root: { innerText: string; querySelectorAll(selector: string): ArrayLike<Scanned> }): string {
  const parts = [root.innerText];
  for (const control of Array.from(root.querySelectorAll("input, textarea, select"))) {
    if (control.value) parts.push(control.value);
    for (const option of Array.from(control.selectedOptions ?? [])) parts.push(option.text);
  }
  for (const element of Array.from(root.querySelectorAll("[title], [placeholder], [aria-label]"))) {
    for (const name of ["title", "placeholder", "aria-label"]) {
      const value = element.getAttribute(name);
      if (value) parts.push(value);
    }
  }
  return parts.join("\n");
}

// underHome says whether a scratch directory lies in one of the home folders, whose path would
// show the user's name on screen.
export function underHome(scratch: string, homes: string[]): boolean {
  const s = normal(scratch);
  return homes.some((h) => h !== "" && (s === normal(h) || s.startsWith(normal(h) + "/")));
}

interface Preflight {
  outcome?: string;
  refused_at?: string;
  reason?: string;
  [key: string]: unknown;
  preflight?: { version?: string; status?: unknown; settings?: string; user_configuration?: unknown[] | null; [key: string]: unknown };
}

// preflightRecord keeps what the evidence needs of ada-live's masked preflight: no names of the
// owner's plugins or MCP servers, only how many there are.
export function preflightRecord(result: Preflight) {
  const pre = result.preflight ?? {};
  return { outcome: result.outcome, refused_at: result.refused_at, reason: result.reason, version: pre.version, status: pre.status, settings: pre.settings, user_configuration_entries: pre.user_configuration?.length ?? 0 };
}
