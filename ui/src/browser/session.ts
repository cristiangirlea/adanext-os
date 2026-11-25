import { AdaError } from "../client/api";

// A refusal that means the browser holds no live session: show the sign-in page.
export function signedOut(e: unknown): boolean {
  return e instanceof AdaError && (e.status === 401 || e.code === "unauthenticated");
}
