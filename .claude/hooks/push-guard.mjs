// A pre-command hook for Bash and PowerShell. It refuses the pushes this repository never makes,
// in the forms permission patterns cannot match reliably: anything that would update main or a
// tag, force, delete or mirror, and a push with no branch named while main (or a detached HEAD) is
// checked out. Feature branches are pushed by name; main changes only through a pull request.
//
// Input is Claude Code's hook JSON on stdin. Exit 2 refuses the command; exit 0 lets it through.
import { execFileSync } from "node:child_process";

let raw = "";
for await (const chunk of process.stdin) raw += chunk;
let input;
try {
  input = JSON.parse(raw);
} catch {
  process.exit(0); // not a tool call this hook understands
}
const command = String(input?.tool_input?.command ?? "");
const cwd = typeof input?.cwd === "string" && input.cwd ? input.cwd : process.cwd();

// Each simple command separately: a push may follow cd, &&, ; or a pipe.
const segments = command.split(/&&|\|\||[;|\n]/);
const reasons = [];
for (const segment of segments) {
  const words = segment.trim().split(/\s+/).filter(Boolean);
  const git = words.findIndex((w) => /^(?:.*[\\/])?git(?:\.exe)?$/i.test(w.replace(/^["']|["']$/g, "")));
  if (git < 0) continue;
  // Skip git's own options (-C dir, -c key=value, --no-pager ...) to find the subcommand.
  let i = git + 1;
  let dir = cwd;
  while (i < words.length && words[i].startsWith("-")) {
    if (words[i] === "-C" && i + 1 < words.length) {
      dir = words[i + 1].replace(/^["']|["']$/g, "");
      i += 2;
    } else if (words[i] === "-c" && i + 1 < words.length) {
      i += 2;
    } else {
      i += 1;
    }
  }
  if (words[i] !== "push") continue;
  const args = words.slice(i + 1).map((w) => w.replace(/^["']|["']$/g, ""));
  for (const a of args) {
    if (/^(-f|--force.*|--mirror|--delete|-d|--tags|--follow-tags|--all|--prune)$/.test(a)) {
      reasons.push(`the option ${a}`);
    } else if (a.startsWith("+")) {
      reasons.push(`the forced refspec ${a}`);
    } else if (/(^|:)(refs\/heads\/)?main$/.test(a)) {
      reasons.push(`the destination ${a}`);
    } else if (/(^|:)refs\/tags\//.test(a)) {
      reasons.push(`the tag ${a}`);
    }
  }
  // Without a refspec, or with HEAD, git pushes whatever is checked out.
  const refspecs = args.filter((a) => !a.startsWith("-")).slice(1);
  if (refspecs.length === 0 || refspecs.some((r) => r === "HEAD" || r === "@")) {
    let branch = "";
    try {
      branch = execFileSync("git", ["-C", dir, "branch", "--show-current"], { encoding: "utf8" }).trim();
    } catch {
      branch = "";
    }
    if (branch === "main" || branch === "") {
      reasons.push(`no feature branch named while ${branch ? "main" : "an unknown or detached HEAD"} is checked out`);
    }
  }
}

if (reasons.length > 0) {
  process.stderr.write(
    `push refused by .claude/hooks/push-guard.mjs: ${reasons.join("; ")}. ` +
      "Push a feature branch by name (git push -u origin <branch>); main changes only through a pull request.\n",
  );
  process.exit(2);
}
process.exit(0);
