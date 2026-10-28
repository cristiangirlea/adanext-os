// Guards for the live demonstration, kept free of Playwright so Vitest can test them.

// The opt-in: exactly "1", as cmd/ada-live reads it.
export function liveEnabled(env: Record<string, string | undefined>): boolean {
  return env.ADANEXT_LIVE === "1";
}

const emailShaped = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;

// personalText names what must not be on screen in a screenshot: e-mail-shaped text, the home
// directory and the user's name. It says what kind it found, never the text itself.
export function personalText(text: string, user = "", home = ""): string[] {
  const found: string[] = [];
  const emails = text.match(emailShaped)?.length ?? 0;
  if (emails) found.push(`${emails} e-mail-shaped text`);
  const lower = text.toLowerCase();
  if (home && [home, home.replaceAll("\\", "/")].some((h) => lower.includes(h.toLowerCase()))) found.push("the home directory");
  if (user.length >= 3 && lower.includes(user.toLowerCase())) found.push("the user name");
  return found;
}
