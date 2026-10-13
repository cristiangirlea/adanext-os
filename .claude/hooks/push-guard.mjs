// A pre-command hook for Bash and PowerShell. In this repository a push names one feature branch
// explicitly, and main changes only through a pull request.
//
// Parsing every way a shell can spell a command (quotes, escapes, continuations, cd, nested
// shells, environment variables) is a race a hook cannot win, so this guard does not try. Any
// command that mentions both git and push must be exactly one plain push, alone:
//
//   git push [-u | --set-upstream | -n | --dry-run]... <remote> <branch>[:<branch>]...
//
// with nothing else on the line: no quotes, escapes, variables, separators, redirections or
// continuations, and no -C or -c. It then runs in the session's working directory, so the guard's
// own git calls see the same repository and the same effective configuration (environment
// overrides included) as the push will. Each branch must be an existing local branch that is not
// also a tag, not main and not HEAD; a destination must be the same name; push.followTags must be
// off; and a branch without a destination is refused while remote.<remote>.push could map it
// elsewhere.
//
// "Mentions a push" means the words push, send-pack or http-push anywhere on the line, which also
// catches git subtree push. Every git subcommand on the line that is not a built-in is looked up
// as an alias, and one that expands to a push or to a shell command is refused, so an alias cannot
// hide a push from these words.
//
// One exception keeps ordinary work possible: a single git command from a short list that never
// pushes (commit, log, show, ...) passes even when push appears in its quoted arguments, such as a
// commit message that mentions git push.
//
// This is a guard rail against Claude's own mistakes, not a security boundary: a push from a script
// file it never sees, or a command crafted to defeat a text check, is out of its reach. The
// boundary for main is the repository's branch protection on GitHub, which requires pull requests.
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
const powershell = /powershell/i.test(String(input?.tool_name ?? ""));

if (!/\bgit\b/i.test(command)) process.exit(0);

// Words that name a way to push: push itself, its plumbing, and a subtree push.
const pushy = /\b(?:push|send-pack|http-push)\b/i;

// Git ignores an alias named like a built-in command, so only other names are looked up. An alias
// that expands to a push, or to a shell command (which could do anything), is refused.
const builtins = new Set(
  ("add am apply archive bisect blame branch bundle checkout cherry-pick clean clone commit config " +
    "describe diff fetch format-patch gc grep help init log ls-files ls-remote merge mv notes pull push " +
    "range-diff rebase reflog remote reset restore revert rev-list rev-parse rm shortlog show sparse-checkout " +
    "stash status submodule switch tag version worktree").split(" "),
);

function refuse(why) {
  process.stderr.write(
    `push refused by .claude/hooks/push-guard.mjs: ${why}. Run the push alone, as ` +
      "git push -u origin <branch>, from the repository's directory; main changes only through a pull request.\n",
  );
  process.exit(2);
}

// GIT_CONFIG redirects only the git config command, so the guard's lookups must not inherit it:
// they have to read what the push itself will read.
const inspectEnv = { ...process.env };
delete inspectEnv.GIT_CONFIG;

function git(args) {
  try {
    const out = execFileSync("git", ["-C", cwd, ...args], {
      encoding: "utf8",
      env: inspectEnv,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 10000,
    });
    return { ok: true, out: out.trim() };
  } catch {
    return { ok: false, out: "" };
  }
}

// Options that make git run a program of the caller's choosing can push from inside any command.
if (/--(?:upload-pack|receive-pack|exec)\b/.test(command)) refuse("an option that makes git run another program");

// Aliases. Git matches built-in names exactly (case-sensitive) and alias names case-insensitively,
// follows an alias that names another alias, and reads them from the repository a -C points at or
// from configuration a -c adds; an alias behind any git option is therefore refused rather than
// resolved.
for (const m of command.matchAll(/\bgit(?:\.exe)?\s+((?:-[Cc]\s+\S+\s+|-\S+\s+)*)([A-Za-z][\w.-]*)/gi)) {
  const [, gitOptions, sub] = m;
  if (builtins.has(sub)) continue;
  if (gitOptions.trim() !== "" && gitOptions.trim() !== "--no-pager") {
    refuse(`git ${sub} behind git options this guard does not resolve`);
  }
  let name = sub;
  for (let depth = 0; depth < 10; depth++) {
    const alias = git(["config", "--get", `alias.${name}`]);
    if (!alias.ok) break; // not an alias: a git extension or a typo, which cannot push by this name
    if (pushy.test(alias.out) || alias.out.startsWith("!")) refuse(`git ${sub} is an alias for "${alias.out}"`);
    const next = alias.out.split(/\s+/)[0];
    if (builtins.has(next)) break;
    name = next;
    if (depth === 9) refuse(`git ${sub} is an alias chain too long to check`);
  }
}

if (!pushy.test(command)) process.exit(0);

const words = command.trim().split(/ +/);
const isPlainPush =
  /^[A-Za-z0-9 ._:\/-]+$/.test(command.trim()) && words[0] === "git" && words[1] === "push";

if (!isPlainPush) {
  // The exception: one git command that cannot push, whose mention of push is only in its
  // arguments (a commit message, a log search).
  if (singleNonPushingGitCommand(command)) process.exit(0);
  refuse("a command that mentions a push is allowed only as one plain git push on its own");
}

const options = words.slice(2).filter((w) => w.startsWith("-"));
const positional = words.slice(2).filter((w) => !w.startsWith("-"));
for (const o of options) {
  if (!["-u", "--set-upstream", "-n", "--dry-run"].includes(o)) refuse(`the option ${o}`);
}
if (positional.length < 2) refuse("no remote and feature branch named");
const [remote, ...refspecs] = positional;
if (!/^[A-Za-z0-9._-]+$/.test(remote)) refuse(`the remote ${remote}`);
if (git(["config", "--type=bool", "--get", "push.followTags"]).out === "true") {
  refuse("push.followTags is on, so tags would go too");
}
const pushConfigured = git(["config", "--get-all", `remote.${remote}.push`]).out !== "";
const plainBranch = /^(?!.*\.\.)(?!.*\/\/)[A-Za-z0-9._\/-]+(?<![.\/])$/;
for (const spec of refspecs) {
  const parts = spec.split(":");
  const name = parts[0];
  if (parts.length > 2 || (parts.length === 2 && parts[1] !== name)) refuse(`the refspec ${spec}`);
  if (!plainBranch.test(name) || name === "main" || name === "HEAD" || name.startsWith("refs/")) {
    refuse(`the refspec ${spec}`);
  }
  if (!git(["show-ref", "--verify", "--quiet", `refs/heads/${name}`]).ok) refuse(`${name} is not a local branch here`);
  if (git(["show-ref", "--verify", "--quiet", `refs/tags/${name}`]).ok) refuse(`${name} is also a tag`);
  if (parts.length === 1 && pushConfigured) {
    refuse(`remote.${remote}.push is configured, so ${name} without an explicit destination may go elsewhere`);
  }
}
process.exit(0);

// singleNonPushingGitCommand reports whether the command is one simple git command, every argument
// of it literal, whose subcommand is on a short list of commands that never push. Literal means:
// no separator, redirection, continuation, escape or substitution anywhere outside quotes, and
// inside double quotes no $ or backtick, since Bash and PowerShell both run $(...) and Bash runs
// `...` there. Single quotes are literal in both shells. The only git option allowed before the
// subcommand is --no-pager. Any doubt answers false.
function singleNonPushingGitCommand(text) {
  const neverPush = new Set(["add", "blame", "branch", "commit", "diff", "fetch", "grep", "log", "ls-files",
    "restore", "rev-parse", "shortlog", "show", "stash", "status", "switch", "tag"]);
  const escape = powershell ? "`" : "\\";
  const out = [];
  let word = null;
  let quote = null;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote) {
      if (quote === '"' && (c === "$" || c === "`" || c === escape)) return false;
      if (c === quote) quote = null;
      else word += c;
      continue;
    }
    if (c === escape || c === "$" || c === "(" || c === ")" || c === "`" || c === "\n") return false;
    if (c === ";" || c === "&" || c === "|" || c === "<" || c === ">") return false;
    if (c === "'" || c === '"') {
      quote = c;
      word = word ?? "";
    } else if (/\s/.test(c)) {
      if (word !== null) out.push(word);
      word = null;
    } else {
      word = (word ?? "") + c;
    }
  }
  if (quote) return false;
  if (word !== null) out.push(word);
  if (!/^git(?:\.exe)?$/i.test(out[0] ?? "")) return false;
  let i = 1;
  if (out[i] === "--no-pager") i++;
  // Any other git option before the subcommand (-c, -C, --exec-path, ...) could change what runs.
  if (out[i] === undefined || out[i].startsWith("-")) return false;
  return neverPush.has(out[i].toLowerCase());
}
