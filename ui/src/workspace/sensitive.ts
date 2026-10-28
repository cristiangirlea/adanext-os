// The same rule as internal/source's sensitive(): a path that looks like a credential by its name
// or by a .aws or .ssh directory above it. A snapshot refuses such an untracked file unless it is
// listed in include_sensitive, so the import flow marks them and leaves them unselected.
export function sensitive(path: string): boolean {
  const parts = path.toLowerCase().split("/");
  const base = parts[parts.length - 1] ?? "";
  if (parts.slice(0, -1).some((dir) => dir === ".aws" || dir === ".ssh")) return true;
  if ([".npmrc", ".netrc", "_netrc", ".pypirc", "credentials", ".git-credentials"].includes(base)) return true;
  if ([".pem", ".key", ".p12", ".pfx", ".kdbx"].some((ext) => base.endsWith(ext))) return true;
  return [".env", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "credentials."].some((prefix) => base.startsWith(prefix));
}
