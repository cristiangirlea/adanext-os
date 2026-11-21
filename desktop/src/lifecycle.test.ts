import { describe, expect, it, vi } from "vitest";
import * as childProcess from "node:child_process";
import path from "node:path";
import { EventEmitter } from "node:events";
import { coordinatorConfig, endpoint, findOrStart, LAUNCH_WAIT_MS, parseSID, spawnCoordinator, waitForExit, type LauncherExit, type LifecycleIO, QUIT_WAIT_MS } from "./adad";

// The fake launcher exits when the fake clock reaches `at` (at once by default, never for Infinity).
function io(answers: boolean[], launcher: { at?: number; exit?: Omit<LauncherExit, "at"> } = {}): LifecycleIO {
  let clock = 0;
  const at = launcher.at ?? 0;
  let finish: (exit: LauncherExit) => void = () => {};
  const exited = new Promise<LauncherExit>((resolve) => { finish = resolve; });
  const tick = () => { if (clock >= at) finish({ ...(launcher.exit ?? { code: 0, signal: null }), at }); };
  tick();
  return { probe: vi.fn(async () => answers.shift() ?? false), spawn: vi.fn(async () => ({ exited })), now: () => clock, sleep: async (ms) => { clock += ms; tick(); } };
}
const config = { binary: "/app/adad", stateDir: "/state/adanext", name: "adad", args: ["-state-dir", "/state/adanext", "-name", "adad"] };

describe("coordinator lifecycle", () => {
  it("spawns hidden and detached with ignored stdio and unreferences the child", async () => {
    const child = Object.assign(new EventEmitter(), { unref: vi.fn() });
    const spawn = vi.fn(() => child as unknown as childProcess.ChildProcess);
    const started = spawnCoordinator(config.binary, config.args, spawn as typeof childProcess.spawn);
    child.emit("spawn");
    await started;
    expect(spawn).toHaveBeenCalledExactlyOnceWith(config.binary, config.args, { detached: true, stdio: "ignore", windowsHide: true });
    expect(child.unref).toHaveBeenCalledOnce();
  });
  it("reports how the launcher it spawned exited", async () => {
    const child = Object.assign(new EventEmitter(), { unref: vi.fn() });
    const spawn = vi.fn(() => child as unknown as childProcess.ChildProcess);
    const started = spawnCoordinator(config.binary, config.args, spawn as typeof childProcess.spawn);
    child.emit("spawn");
    const launcher = await started;
    const before = Date.now();
    child.emit("exit", 3, null);
    await expect(launcher.exited).resolves.toEqual({ code: 3, signal: null, at: expect.any(Number) });
    expect((await launcher.exited).at).toBeGreaterThanOrEqual(before);
    // An exit that comes before anyone awaits the launch is kept too.
    const early = Object.assign(new EventEmitter(), { unref: vi.fn() });
    const pending = spawnCoordinator(config.binary, config.args, vi.fn(() => early as unknown as childProcess.ChildProcess) as typeof childProcess.spawn);
    early.emit("spawn");
    early.emit("exit", null, "SIGTERM");
    await expect((await pending).exited).resolves.toMatchObject({ code: null, signal: "SIGTERM" });
  });
  it("counts the wait from the launcher's exit, however long the launcher takes", async () => {
    // A cold Windows PowerShell start behind the broker took 30 s on a busy machine.
    const slow = io([], { at: 40_000 });
    const answers = [false];
    slow.probe = vi.fn(async () => answers.shift() ?? slow.now() >= 50_000);
    await findOrStart(config, "/pipe", slow);
    expect(slow.spawn).toHaveBeenCalledOnce();
    expect(slow.now()).toBeGreaterThanOrEqual(50_000);
    // The serving adad then has 15 s from the launcher's exit, not from the spawn.
    const silent = io([], { at: 40_000 });
    await expect(findOrStart(config, "/pipe", silent)).rejects.toThrow("timed out waiting for adad to start;");
    expect(silent.now()).toBe(55_000);
    // An endpoint that answers while the launcher still runs is ready.
    const early = io([false, false, true], { at: Infinity });
    await findOrStart(config, "/pipe", early);
    expect(early.now()).toBeLessThan(1000);
  });
  it("fails at once when the launcher fails, naming the start-up log", async () => {
    const failed = io([], { at: 300, exit: { code: 1, signal: null } });
    await expect(findOrStart(config, "/pipe", failed)).rejects.toThrow(`adad's launcher failed (exit code 1); its start-up log is ${path.join(config.stateDir, "startup.log")}`);
    expect(failed.now()).toBeLessThan(1000);
    const killed = io([], { at: 300, exit: { code: null, signal: "SIGKILL" } });
    await expect(findOrStart(config, "/pipe", killed)).rejects.toThrow("adad's launcher failed (signal SIGKILL)");
    // After a launch that succeeded, an access error still ends the wait rather than being retried.
    const denied = io([]);
    const budgets: number[] = [];
    denied.probe = vi.fn(async (_socket: string, timeoutMs: number) => {
      budgets.push(timeoutMs);
      if (budgets.length < 3) return false;
      throw Object.assign(new Error("access denied"), { code: "EACCES" });
    });
    await expect(findOrStart(config, "/pipe", denied, 300)).rejects.toThrow("access denied");
    // The third probe runs in the serving adad's 300 ms, after the launcher's exit was seen.
    expect(budgets).toHaveLength(3);
    expect(budgets[2]).toBeLessThanOrEqual(300);
  });
  it("bounds a launcher that never finishes", async () => {
    const stuck = io([], { at: Infinity });
    await expect(findOrStart(config, "/pipe", stuck)).rejects.toThrow(`timed out waiting for adad's launcher to finish; its start-up log is ${path.join(config.stateDir, "startup.log")}`);
    expect(stuck.now()).toBe(75_000);
    expect(LAUNCH_WAIT_MS).toBe(75_000);
    // An exit at the limit still counts; one after it is a launcher that did not finish in time.
    const atLimit = io([], { at: 75_000 });
    await expect(findOrStart(config, "/pipe", atLimit)).rejects.toThrow("timed out waiting for adad to start;");
    expect(atLimit.now()).toBe(90_000);
    // A probe that resumes after the limit, with an exit that came in meanwhile at 75_001.
    let clock = 0;
    let finish: (exit: LauncherExit) => void = () => {};
    const exited = new Promise<LauncherExit>((resolve) => { finish = resolve; });
    const late: LifecycleIO = {
      probe: async () => { if (clock >= 74_900 && clock < 75_000) { clock = 75_020; finish({ code: 0, signal: null, at: 75_001 }); } return false; },
      spawn: async () => ({ exited }), now: () => clock, sleep: async (ms) => { clock += ms; },
    };
    await expect(findOrStart(config, "/pipe", late)).rejects.toThrow("timed out waiting for adad's launcher to finish;");
  });
  it("finds a running coordinator without starting one", async () => {
    const fake = io([true]);
    await findOrStart(config, "/pipe", fake);
    expect(fake.spawn).not.toHaveBeenCalled();
  });
  it("starts detached and waits for the endpoint, including a competing singleton launch", async () => {
    const fake = io([false, false, true]);
    await findOrStart(config, "/pipe", fake);
    expect(fake.spawn).toHaveBeenCalledExactlyOnceWith(config.binary, config.args);
    expect(fake.probe).toHaveBeenCalledTimes(3);
  });
  it("bounds startup and propagates launch and access errors", async () => {
    // A detached coordinator's stderr is null: the message names where its start-up errors are.
    await expect(findOrStart(config, "/pipe", io([]), 100)).rejects.toThrow(`timed out waiting for adad to start; its start-up log is ${path.join(config.stateDir, "startup.log")}`);
    const denied = io([]);
    denied.probe = async () => { throw new Error("access denied"); };
    await expect(findOrStart(config, "/pipe", denied)).rejects.toThrow("access denied");
    expect(denied.spawn).not.toHaveBeenCalled();
    const missing = io([false]);
    missing.spawn = async () => { throw new Error("missing binary"); };
    await expect(findOrStart(config, "/pipe", missing)).rejects.toThrow("missing binary");
  });
  it("keeps waiting while a coordinator it launched times out, and only then", async () => {
    const timedOut = () => Object.assign(new Error("adad did not answer in time"), { code: "ETIMEDOUT" });
    const starting = io([]);
    const answers: (boolean | Error)[] = [false, timedOut(), timedOut(), true];
    starting.probe = vi.fn(async () => { const next = answers.shift() ?? false; if (next instanceof Error) throw next; return next; });
    await findOrStart(config, "/pipe", starting);
    expect(starting.spawn).toHaveBeenCalledOnce();
    expect(starting.probe).toHaveBeenCalledTimes(4);
    const hung = io([]);
    hung.probe = async () => { throw timedOut(); };
    await expect(findOrStart(config, "/pipe", hung)).rejects.toThrow("adad did not answer in time");
    expect(hung.spawn).not.toHaveBeenCalled();
    const neverReady = io([]);
    let launched = false;
    neverReady.probe = async () => { if (!launched) { launched = true; return false; } throw timedOut(); };
    const failure = await findOrStart(config, "/pipe", neverReady, 1000).catch((error: Error) => error);
    expect(failure).toBeInstanceOf(Error);
    expect((failure as Error).message).toBe(`timed out waiting for adad to start: the endpoint accepted connections but did not answer; its start-up log is ${path.join(config.stateDir, "startup.log")}`);
    expect(((failure as Error).cause as NodeJS.ErrnoException).code).toBe("ETIMEDOUT");
    expect(neverReady.spawn).toHaveBeenCalledOnce();
  });
  it("waits for disappearance without starting anything and bounds the wait", async () => {
    const fake = io([true, true, false]);
    await waitForExit("/pipe", fake, 1000);
    expect(fake.spawn).not.toHaveBeenCalled();
    const stuck = io([]); stuck.probe = async () => true;
    await expect(waitForExit("/pipe", stuck, 100)).rejects.toThrow(/timed out/);
  });
  it("uses a tested SID parser and the per-user endpoint names", () => {
    expect(parseSID('"host\\owner","S-1-5-21-123-456-789-1001"\r\n')).toBe("S-1-5-21-123-456-789-1001");
    expect(() => parseSID("garbage")).toThrow();
    expect(() => parseSID('"x","S-1-5-21-1"\n"y","S-1-5-21-2"')).toThrow();
    expect(endpoint(config, "linux")).toBe("/state/adanext/run/adad.sock");
    expect(endpoint(config, "win32", "S-1-5-21-1")).toBe("\\\\.\\pipe\\adanext-adad-S-1-5-21-1");
    expect(() => endpoint({ ...config, name: "../other" }, "linux")).toThrow();
    expect(() => endpoint(config, "win32")).toThrow();
  });
  it("allows the default executor stop budget plus fifteen seconds and bounds a stuck endpoint", async () => {
    const draining = io([]);
    draining.probe = async () => draining.now() < 65_000;
    await waitForExit("/pipe", draining);
    expect(draining.now()).toBe(65_000);
    const stuck = io([]);
    stuck.probe = async () => true;
    await expect(waitForExit("/pipe", stuck)).rejects.toThrow("timed out waiting for adad to stop");
    expect(stuck.now()).toBe(QUIT_WAIT_MS);
  });
  it("uses the user state directory and passes through only configured engine flags", () => {
    const env = { LOCALAPPDATA: "C:\\Users\\owner\\AppData\\Local", ADANEXT_ADAD: "C:\\app\\adad.exe", ADANEXT_ENGINE: "C:\\app\\hrn.exe", ADANEXT_ENGINE_SHA256: "a".repeat(64) };
    const c = coordinatorConfig(env, "C:\\app", "C:\\Users\\owner", "win32");
    expect(c.binary).toBe(env.ADANEXT_ADAD);
    expect(c.stateDir).toBe("C:\\Users\\owner\\AppData\\Local\\AdaNext");
    expect(c.args).toEqual(["-state-dir", c.stateDir, "-name", "adad", "-detach", "-engine", env.ADANEXT_ENGINE, "-engine-sha256", env.ADANEXT_ENGINE_SHA256]);
    expect(coordinatorConfig({ XDG_STATE_HOME: "relative" }, "/app", "/home/u", "linux").stateDir).toBe("/home/u/.local/state/adanext");
    expect(coordinatorConfig({ XDG_STATE_HOME: "/state" }, "/app", "/home/u", "linux").binary).toBe("/app/adad");
  });
});
