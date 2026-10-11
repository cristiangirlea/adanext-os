// A pre-command hook for Bash and PowerShell. Pushes in this repository name one feature branch
// explicitly; main changes only through a pull request. Rather than list forbidden forms, which
// always misses one, it allows exactly
//
//   git [-C <dir>] push [-u | --set-upstream | -n | --dry-run]... <remote> <branch>[:<branch>]...
//
// where each branch is a plain name of an existing local branch that is not also a tag, not main
// and not HEAD; both sides of a refspec are the same name; no configuration override is given on
// the command line or in GIT_CONFIG_* variables; push.followTags is off; and a refspec without a
// destination is refused when remote.<remote>.push could map it elsewhere. Commands are split the
// way the shell would (quotes, escapes, separators), and a push inside a command string handed to
// a shell (sh -c, bash -c, pwsh -Command, cmd /c) is checked the same way. Anything this hook
// cannot parse with certainty, if it might be a push, is refused.
//
// This guards what Claude runs directly. It is a guard rail, not a sandbox: a push from inside a
// script file it never sees is out of its reach.
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

const reasons = [];
const refuse = (why) => reasons.push(why);
const mayBePush = (text) => /\bgit(?:\.exe)?\b[\s\S]*\bpush\b/i.test(text);

// tokenize splits a command line into simple commands, each a list of words, honouring quotes
// and escapes: a backslash in Bash, a backtick in PowerShell. It returns null for an unbalanced
// quote. Separators are ; & && || | and newlines outside quotes.
function tokenize(text) {
  const escape = powershell ? "`" : "\\";
  const segments = [[]];
  let word = null;
  let quote = null;
  const endWord = () => {
    if (word !== null) segments[segments.length - 1].push(word);
    word = null;
  };
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote) {
      if (c === quote) quote = null;
      else if (c === escape && quote === '"' && i + 1 < text.length) word += text[++i];
      else word += c;
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      word = word ?? "";
    } else if (c === escape && i + 1 < text.length) {
      word = (word ?? "") + text[++i];
    } else if (/\s/.test(c) && c !== "\n") {
      endWord();
    } else if (c === ";" || c === "&" || c === "|" || c === "\n") {
      endWord();
      if (segments[segments.length - 1].length > 0) segments.push([]);
    } else {
      word = (word ?? "") + c;
    }
  }
  if (quote) return null;
  endWord();
  return segments.filter((s) => s.length > 0);
}

function git(dir, args) {
  try {
    return { ok: true, out: execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim() };
  } catch {
    return { ok: false, out: "" };
  }
}

const shells = /^(?:.*[\\/])?(?:sh|bash|dash|zsh|pwsh|powershell|cmd)(?:\.exe)?$/i;
const shellCommandFlag = /^(?:-c|-lc|-ic|-Command|-c(?:ommand)?|\/c|\/k)$/i;
const isGit = (w) => /^(?:.*[\\/])?git(?:\.exe)?$/i.test(w);
const allowedOptions = new Set(["-u", "--set-upstream", "-n", "--dry-run"]);
const plainBranch = /^(?!-)(?!.*\.\.)(?!.*\/\/)(?!.*@\{)[A-Za-z0-9._\/-]+(?<![.\/])$/;

function checkCommandLine(text, depth) {
  const segments = tokenize(text);
  if (segments === null) {
    if (mayBePush(text)) refuse("a command with an unbalanced quote that may push");
    return;
  }
  for (const words of segments) {
    // A command string handed to a shell is checked as a command line of its own.
    if (shells.test(words[0] ?? "")) {
      const flag = words.findIndex((w, i) => i > 0 && shellCommandFlag.test(w));
      if (flag > 0 && flag + 1 < words.length) {
        if (depth >= 3) refuse("shell commands nested too deeply to check");
        else checkCommandLine(words.slice(flag + 1).join(" "), depth + 1);
      } else if (words.slice(1).some(mayBePush)) {
        refuse("a shell invocation this guard cannot read that may push");
      }
      continue;
    }
    const g = words.findIndex(isGit);
    if (g < 0) continue;
    let i = g + 1;
    let dir = cwd;
    let overrides = false;
    let unknown = false;
    while (i < words.length && words[i].startsWith("-")) {
      if (words[i] === "-C" && i + 1 < words.length) {
        dir = words[i + 1];
        i += 2;
      } else if (words[i] === "-c" || words[i].startsWith("--config-env")) {
        overrides = true;
        i += words[i] === "-c" ? 2 : 1;
      } else if (words[i] === "--no-pager") {
        i += 1;
      } else {
        unknown = true;
        i += 1;
      }
    }
    if (words[i] !== "push") continue;
    if (unknown) refuse("a git option before push that this guard does not know");
    if (overrides) refuse("a configuration override on the command line");
    if (/GIT_CONFIG_(?:COUNT|PARAMETERS|KEY_|VALUE_|GLOBAL|SYSTEM)|GIT_DIR|GIT_WORK_TREE/.test(command)) {
      refuse("GIT_CONFIG or GIT_DIR variables set for a push");
    }
    // Redirections (2>&1, > out.txt) belong to the shell, not to git.
    const args = words.slice(i + 1).filter((w) => !/^\d*[<>]/.test(w));
    for (const o of args.filter((a) => a.startsWith("-"))) {
      if (!allowedOptions.has(o)) refuse(`the option ${o}`);
    }
    const positional = args.filter((a) => !a.startsWith("-"));
    if (positional.length < 2) {
      refuse("no remote and feature branch named (push as: git push -u origin <branch>)");
      continue;
    }
    const [remote, ...refspecs] = positional;
    if (!/^[A-Za-z0-9._-]+$/.test(remote)) refuse(`the remote ${remote}`);
    const followTags = git(dir, ["config", "--type=bool", "--get", "push.followTags"]);
    if (followTags.out === "true") refuse("push.followTags is on, so tags would go too");
    const pushConfigured = git(dir, ["config", "--get-all", `remote.${remote}.push`]).out !== "";
    for (const spec of refspecs) {
      const parts = spec.split(":");
      const shapeOK = parts.length <= 2 && (parts.length === 1 || parts[0] === parts[1]);
      const name = parts[0];
      if (!shapeOK || !plainBranch.test(name) || name === "main" || name === "HEAD" || name.startsWith("refs/")) {
        refuse(`the refspec ${spec}`);
        continue;
      }
      if (!git(dir, ["show-ref", "--verify", "--quiet", `refs/heads/${name}`]).ok) {
        refuse(`${name}, which is not a local branch here`);
      } else if (git(dir, ["show-ref", "--verify", "--quiet", `refs/tags/${name}`]).ok) {
        refuse(`${name}, which is also a tag`);
      }
      if (parts.length === 1 && pushConfigured) {
        refuse(`remote.${remote}.push is configured, so ${name} without an explicit destination may go elsewhere`);
      }
    }
  }
}

checkCommandLine(command, 0);

if (reasons.length > 0) {
  process.stderr.write(
    `push refused by .claude/hooks/push-guard.mjs: ${reasons.join("; ")}. ` +
      "Push one feature branch by name (git push -u origin <branch>); main changes only through a pull request.\n",
  );
  process.exit(2);
}
process.exit(0);
