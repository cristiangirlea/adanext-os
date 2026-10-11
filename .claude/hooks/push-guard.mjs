// A pre-command hook for Bash and PowerShell. Pushes in this repository name one feature branch
// explicitly; main changes only through a pull request. So instead of listing forbidden forms,
// which always misses one (clustered flags like -uf, the matching refspec ":", wildcards), this
// allows exactly:
//
//   git [-C <dir>] push [-u | --set-upstream | -n | --dry-run]... <remote> <branch>[:<branch>]...
//
// where every branch is a plain name (no wildcard, no "refs/", not HEAD, not main), both sides of a
// refspec are the same name, and the remote has no configured push refspec that could map it
// elsewhere. Any other push, including one this hook cannot parse, is refused.
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

const unquote = (w) => w.replace(/^(["'])(.*)\1$/, "$2");
const allowedOptions = new Set(["-u", "--set-upstream", "-n", "--dry-run"]);
const branchName = /^(?!-)(?!.*\.\.)(?!.*\/\/)(?!.*@\{)[A-Za-z0-9._\/-]+(?<![.\/])$/;
const forbiddenBranch = (b) => b === "main" || b === "HEAD" || b.startsWith("refs/");

function configuredPush(dir, remote) {
  try {
    return execFileSync("git", ["-C", dir, "config", "--get-all", `remote.${remote}.push`], {
      encoding: "utf8",
    }).trim();
  } catch {
    return ""; // exit 1: no such setting
  }
}

const reasons = [];
// Each simple command separately: a push may follow cd, &&, ; or a pipe.
for (const segment of command.split(/&&|\|\||[;|\n]/)) {
  const words = segment.trim().split(/\s+/).filter(Boolean).map(unquote);
  const git = words.findIndex((w) => /^(?:.*[\\/])?git(?:\.exe)?$/i.test(w));
  if (git < 0) continue;
  let i = git + 1;
  let dir = cwd;
  let unknownGitOption = false;
  while (i < words.length && words[i].startsWith("-")) {
    if (words[i] === "-C" && i + 1 < words.length) {
      dir = words[i + 1];
      i += 2;
    } else if (words[i] === "-c" && i + 1 < words.length) {
      i += 2;
    } else if (words[i] === "--no-pager") {
      i += 1;
    } else {
      unknownGitOption = true;
      i += 1;
    }
  }
  if (words[i] !== "push") continue;
  if (unknownGitOption) {
    reasons.push("a git option before push that this guard does not know");
    continue;
  }
  // Redirections (2>&1, > out.txt) belong to the shell, not to git.
  const args = words.slice(i + 1).filter((w) => !/^\d*[<>]/.test(w));
  const options = args.filter((a) => a.startsWith("-"));
  const positional = args.filter((a) => !a.startsWith("-"));
  for (const o of options) {
    if (!allowedOptions.has(o)) reasons.push(`the option ${o}`);
  }
  if (positional.length < 2) {
    reasons.push("no remote and feature branch named (push as: git push -u origin <branch>)");
    continue;
  }
  const [remote, ...refspecs] = positional;
  if (!/^[A-Za-z0-9._-]+$/.test(remote)) {
    reasons.push(`the remote ${remote}`);
  } else if (configuredPush(dir, remote)) {
    reasons.push(`remote.${remote}.push is configured, so the destination is not what the command says`);
  }
  for (const spec of refspecs) {
    const parts = spec.split(":");
    const ok =
      parts.length <= 2 &&
      parts.every((p) => branchName.test(p) && !forbiddenBranch(p)) &&
      (parts.length === 1 || parts[0] === parts[1]);
    if (!ok) reasons.push(`the refspec ${spec}`);
  }
}

if (reasons.length > 0) {
  process.stderr.write(
    `push refused by .claude/hooks/push-guard.mjs: ${reasons.join("; ")}. ` +
      "Push one feature branch by name (git push -u origin <branch>); main changes only through a pull request.\n",
  );
  process.exit(2);
}
process.exit(0);
