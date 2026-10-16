import { describe, expect, it, vi } from "vitest";
import * as childProcess from "node:child_process";
import { EventEmitter } from "node:events";
import { coordinatorConfig, endpoint, findOrStart, parseSID, spawnCoordinator, waitForExit, type LifecycleIO, QUIT_WAIT_MS } from "./adad";

function io(answers: boolean[]): LifecycleIO {
  let clock = 0;
  return { probe: vi.fn(async () => answers.shift() ?? false), spawn: vi.fn(async () => {}), now: () => clock, sleep: async (ms) => { clock += ms; } };
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
    await expect(findOrStart(config, "/pipe", io([]), 100)).rejects.toThrow(/timed out/);
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
    await expect(findOrStart(config, "/pipe", neverReady, 1000)).rejects.toThrow("timed out waiting for adad to start");
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
