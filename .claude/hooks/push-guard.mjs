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
// One exception keeps ordinary work possible: a single git command that is not a push (a commit
// whose message mentions "git push", say) passes, because its quoted arguments are data.
//
// This guards what Claude runs directly. It is a guard rail, not a sandbox: a push from a script
// file it never sees is out of its reach.
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

if (!/\bgit\b/i.test(command) || !/\bpush\b/i.test(command)) process.exit(0);

function refuse(why) {
  process.stderr.write(
    `push refused by .claude/hooks/push-guard.mjs: ${why}. Run the push alone, as ` +
      "git push -u origin <branch>, from the repository's directory; main changes only through a pull request.\n",
  );
  process.exit(2);
}

function git(args) {
  try {
    const out = execFileSync("git", ["-C", cwd, ...args], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 10000,
    });
    return { ok: true, out: out.trim() };
  } catch {
    return { ok: false, out: "" };
  }
}

const words = command.trim().split(/ +/);
const isPlainPush =
  /^[A-Za-z0-9 ._:\/-]+$/.test(command.trim()) && words[0] === "git" && words[1] === "push";

if (!isPlainPush) {
  // The exception: one git command, not a push, whose only mention of push is in its arguments.
  if (singleGitCommandThatIsNotAPush(command)) process.exit(0);
  refuse("a command that mentions git and push is allowed only as one plain push on its own");
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

// singleGitCommandThatIsNotAPush reports whether the command is one simple git command (no
// separator, continuation or substitution outside quotes) whose subcommand is not push. Quotes
// follow the shell: in Bash a backslash escapes outside single quotes; in PowerShell a backtick
// does. Any doubt answers false.
function singleGitCommandThatIsNotAPush(text) {
  const escape = powershell ? "`" : "\\";
  const out = [];
  let word = null;
  let quote = null;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote) {
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
  const sub = out.slice(1).find((w) => !w.startsWith("-"));
  // git's own options before the subcommand could change what runs; allow none.
  if (out[1]?.startsWith("-")) return false;
  return sub !== undefined && sub.toLowerCase() !== "push";
}
