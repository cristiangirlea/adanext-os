import { AdaError } from "../api";

// The browser's passkey ceremonies: adad's JSON options in, the authenticator's credential out as
// the JSON adad parses. The JSON helpers of PublicKeyCredential are used where the browser has
// them; otherwise the binary fields are converted from and to base64url here. Only the adapters
// may reach navigator.credentials (ui/src/boundary.test.ts).

type Json = Record<string, unknown>;

export function fromBase64url(text: string): ArrayBuffer {
  const base64 = text.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes.buffer;
}

export function toBase64url(buffer: ArrayBuffer | ArrayBufferView): string {
  const bytes = buffer instanceof ArrayBuffer ? new Uint8Array(buffer) : new Uint8Array(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function record(value: unknown): Json {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new AdaError("bad_response", 0, "adad's passkey options are not an object");
  return value as Json;
}

function descriptors(value: unknown): Json[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((d) => {
    const entry = record(d);
    return { ...entry, id: fromBase64url(String(entry.id)) };
  });
}

interface JsonParsers {
  parseCreationOptionsFromJSON?: (options: unknown) => PublicKeyCredentialCreationOptions;
  parseRequestOptionsFromJSON?: (options: unknown) => PublicKeyCredentialRequestOptions;
}

function parsers(): JsonParsers {
  return (globalThis as { PublicKeyCredential?: JsonParsers }).PublicKeyCredential ?? {};
}

export function creationOptions(json: unknown): PublicKeyCredentialCreationOptions {
  const parse = parsers().parseCreationOptionsFromJSON;
  if (typeof parse === "function") return parse(json);
  const options = record(json);
  const user = record(options.user);
  const converted: Json = { ...options, challenge: fromBase64url(String(options.challenge)), user: { ...user, id: fromBase64url(String(user.id)) } };
  const exclude = descriptors(options.excludeCredentials);
  if (exclude) converted.excludeCredentials = exclude;
  return converted as unknown as PublicKeyCredentialCreationOptions;
}

export function requestOptions(json: unknown): PublicKeyCredentialRequestOptions {
  const parse = parsers().parseRequestOptionsFromJSON;
  if (typeof parse === "function") return parse(json);
  const options = record(json);
  const converted: Json = { ...options, challenge: fromBase64url(String(options.challenge)) };
  const allow = descriptors(options.allowCredentials);
  if (allow) converted.allowCredentials = allow;
  return converted as unknown as PublicKeyCredentialRequestOptions;
}

interface CredentialLike {
  id: string;
  rawId: ArrayBuffer;
  type: string;
  authenticatorAttachment?: string | null;
  response: Json;
  toJSON?: () => unknown;
  getClientExtensionResults?: () => unknown;
}

// credentialJSON is the credential as adad parses it.
export function credentialJSON(credential: CredentialLike): unknown {
  if (typeof credential.toJSON === "function") return credential.toJSON();
  const response: Json = {};
  const r = credential.response;
  for (const field of ["clientDataJSON", "attestationObject", "authenticatorData", "signature", "userHandle"]) {
    const value = r[field];
    if (value instanceof ArrayBuffer || ArrayBuffer.isView(value)) response[field] = toBase64url(value);
  }
  if (typeof r.getTransports === "function") response.transports = (r.getTransports as () => string[]).call(r);
  const out: Json = { id: credential.id, rawId: toBase64url(credential.rawId), type: credential.type, response, clientExtensionResults: credential.getClientExtensionResults?.() ?? {} };
  if (credential.authenticatorAttachment) out.authenticatorAttachment = credential.authenticatorAttachment;
  return out;
}

function unavailable(e: unknown): AdaError {
  const why = e instanceof Error && e.message ? e.message : "no passkey was used";
  return new AdaError("passkey_unavailable", 0, `the passkey was not used: ${why}`);
}

function container(method: "get" | "create"): CredentialsContainer {
  const credentials = typeof navigator === "undefined" ? undefined : navigator.credentials;
  if (!credentials || typeof credentials[method] !== "function") throw new AdaError("passkey_unavailable", 0, "this browser offers no passkeys here; sign in with a password where the account allows one");
  return credentials;
}

export async function getPasskey(options: unknown): Promise<unknown> {
  const credentials = container("get");
  let credential: Credential | null;
  try {
    credential = await credentials.get({ publicKey: requestOptions(options) });
  } catch (e) {
    if (e instanceof AdaError) throw e;
    throw unavailable(e);
  }
  if (credential === null) throw unavailable(null);
  return credentialJSON(credential as unknown as CredentialLike);
}

export async function createPasskey(options: unknown): Promise<unknown> {
  const credentials = container("create");
  let credential: Credential | null;
  try {
    credential = await credentials.create({ publicKey: creationOptions(options) });
  } catch (e) {
    if (e instanceof AdaError) throw e;
    throw unavailable(e);
  }
  if (credential === null) throw unavailable(null);
  return credentialJSON(credential as unknown as CredentialLike);
}
