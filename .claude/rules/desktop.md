---
paths:
  - "desktop/**"
---
# Desktop shell

Follow Electron's security guidance (https://www.electronjs.org/docs/latest/tutorial/security).
These hold for every window; the post-edit hook refuses the worst violations.

- `contextIsolation: true`, `sandbox: true`, `nodeIntegration: false`, `webSecurity` left on.
- The renderer gets a small typed API through the preload bridge. No filesystem, shell, database
  or credential access, and no arbitrary host paths or commands: only typed task and executor
  requests.
- Every IPC handler checks the sender's frame and origin before acting.
- Project previews load in separate sandboxed content with no preload bridge and no Ada API.
- A content security policy on every Ada page; no remote code.
- Electron's main process talks to `adad` over the per-user named pipe or owner-only Unix socket.
  Closing a window never stops `adad`; only the explicit "Quit Ada and stop work" action does.
- Versions are exact pins in `package.json` with a committed lockfile. Change a pin only in a slice
  that says why, and prefer a release at least two weeks old.
- Tests: Vitest for components and logic, Playwright for the real workflow against a real `adad`.
  Cap workers (`--maxWorkers`, `workers`) for the lab runner's 3 GiB limit. Milestone demos use the
  real coordinator; fixtures are for tests only.
