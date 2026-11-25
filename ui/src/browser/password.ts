// The server's rule for a new password (internal/api validPassword): at least 12 code points, at
// most 1,024 UTF-8 bytes, and valid text, so no lone surrogate.
export const minPasswordCodePoints = 12;
export const maxPasswordBytes = 1024;

export function validPassword(password: string): boolean {
  return [...password].length >= minPasswordCodePoints && new TextEncoder().encode(password).length <= maxPasswordBytes && !/[\uD800-\uDFFF]/u.test(password);
}
