import type { ReactNode } from "react";

// Text from a repository or an agent (paths, patch lines, file contents, logs, event text) is
// shown as text, never as HTML, and the characters that hide or reorder what a reader sees are
// shown as marks: controls (but tab), the bidirectional controls, format and other invisible
// characters, line and paragraph separators, and the Hangul fillers. Each mark names its code
// point, so "a\u202eb" reads as "a[U+202E]b" and a reviewer sees the line is not what it seems.

const names: Record<number, string> = {
  0x061c: "arabic letter mark",
  0x200b: "zero width space",
  0x200c: "zero width non-joiner",
  0x200d: "zero width joiner",
  0x200e: "left-to-right mark",
  0x200f: "right-to-left mark",
  0x202a: "left-to-right embedding",
  0x202b: "right-to-left embedding",
  0x202c: "pop directional formatting",
  0x202d: "left-to-right override",
  0x202e: "right-to-left override",
  0x2060: "word joiner",
  0x2066: "left-to-right isolate",
  0x2067: "right-to-left isolate",
  0x2068: "first strong isolate",
  0x2069: "pop directional isolate",
  0xfeff: "zero width no-break space",
};

// Controls (Cc), format characters (Cf: the bidirectional controls, zero-width characters, the
// soft hyphen, the tag characters, invisible operators, interlinear annotations), the line and
// paragraph separators (Zl, Zp), and the Hangul fillers, which draw as blank letters. Every
// named character above is in Cf. Text is scanned with one regular expression, not a test per
// character: an attempt's activity re-renders often and must stay cheap.
const hiddenClass = "[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}\\u115F\\u1160\\u3164\\uFFA0\\u180E]";
const hiddenOne = new RegExp(hiddenClass, "u");

function kept(ch: string, keepNewlines: boolean): boolean {
  return ch === "\t" || (keepNewlines && ch === "\n");
}

export function isHidden(code: number, keepNewlines = false): boolean {
  const ch = String.fromCodePoint(code);
  return !kept(ch, keepNewlines) && hiddenOne.test(ch);
}

// hiddenMatches yields each hidden character of a text with its index.
function* hiddenMatches(text: string, keepNewlines: boolean): Generator<{ index: number; ch: string }> {
  for (const m of text.matchAll(new RegExp(hiddenClass, "gu"))) {
    if (!kept(m[0], keepNewlines)) yield { index: m.index, ch: m[0] };
  }
}

function label(code: number): string {
  return `U+${code.toString(16).toUpperCase().padStart(4, "0")}`;
}

function describe(code: number): string {
  if (names[code]) return names[code];
  if (code === 0x7f) return "delete";
  if (code < 0x20 || (code >= 0x80 && code < 0xa0)) return "control character";
  return "invisible character";
}

// hiddenIn lists the hidden characters of a text, once each, in the order first met.
export function hiddenIn(text: string, keepNewlines = false): string[] {
  const found: string[] = [];
  for (const { ch } of hiddenMatches(text, keepNewlines)) {
    const name = label(ch.codePointAt(0)!);
    if (!found.includes(name)) found.push(name);
  }
  return found;
}

// Marked renders text with each hidden character as a visible mark. With keepNewlines, a line
// break stays a line break (file contents, logs).
export function Marked({ text, keepNewlines = false }: { text: string; keepNewlines?: boolean }) {
  const parts: ReactNode[] = [];
  let at = 0;
  let n = 0;
  for (const { index, ch } of hiddenMatches(text, keepNewlines)) {
    if (index > at) parts.push(text.slice(at, index));
    const code = ch.codePointAt(0)!;
    parts.push(
      <mark key={n++} className="hidden-char" title={describe(code)} aria-label={`${describe(code)} ${label(code)}`}>
        {label(code)}
      </mark>,
    );
    at = index + ch.length;
  }
  if (parts.length === 0) return <>{text}</>;
  if (at < text.length) parts.push(text.slice(at));
  return <>{parts}</>;
}

// A path as git reported it: marked, and flagged when its bytes are not UTF-8 (the string is
// then git's text with replacement characters, and the raw bytes are in base64).
export function PathText({ path, raw }: { path: string; raw?: string }) {
  const hidden = hiddenIn(path);
  return (
    <span className="path" data-hidden={hidden.length > 0 || undefined}>
      <Marked text={path} />
      {hidden.length > 0 && <span className="path-warning">hidden characters</span>}
      {raw !== undefined && <span className="path-warning">not UTF-8</span>}
    </span>
  );
}
