// A check is an argv list, run with no shell (M4.2). The form takes a command line and splits it
// on spaces: "double" and 'single' quotes group, and outside single quotes a backslash escapes a
// quote, a space or a backslash (any other backslash stays, so C:\path reads as typed). Nothing is expanded: no globs, no
// variables, no pipes; a "|" is just an argument. An unclosed quote is an error.
export function parseArgv(line: string): string[] | { error: string } {
  const out: string[] = [];
  let word = "";
  let inWord = false;
  let quote: '"' | "'" | null = null;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i]!;
    if (quote === "'") {
      if (ch === "'") quote = null;
      else word += ch;
      continue;
    }
    if (ch === "\\" && i + 1 < line.length && `"' \\`.includes(line[i + 1]!)) {
      word += line[++i];
      inWord = true;
      continue;
    }
    if (quote === '"') {
      if (ch === '"') quote = null;
      else word += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      inWord = true;
      continue;
    }
    if (ch === " " || ch === "\t") {
      if (inWord) out.push(word);
      word = "";
      inWord = false;
      continue;
    }
    word += ch;
    inWord = true;
  }
  if (quote) return { error: `a ${quote === '"' ? "double" : "single"} quote is not closed` };
  if (inWord) out.push(word);
  return out;
}
