// What the screenshots and the interface's flow tests share: `kubehenge serve --demo` started on a
// loopback port, the demo's clock and the browser set-up that reads it the same way.
import { type ChildProcess, spawn } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";

// The demo's clock, which the browser's is set to as well, so that ages read the same in both.
export const demoTime = "2026-10-13T09:00:00Z";
export const viewport = { width: 1440, height: 900 };
// The server listens on 127.0.0.1 only. The browser reaches it as localhost, which it treats as a
// secure origin, so that the session cookie (marked Secure) is kept over plain HTTP.
export const hostResolverRule = "--host-resolver-rules=MAP localhost 127.0.0.1";

const startDeadline = 30_000;
const pollInterval = 200;

export const demoOrigin = (port: string): string => `http://localhost:${port}`;

async function waitForServer(server: ChildProcess, port: string): Promise<void> {
  const until = Date.now() + startDeadline;
  for (;;) {
    if (server.exitCode !== null) {
      throw new Error(`kubehenge serve --demo exited with ${server.exitCode}`);
    }
    try {
      const res = await fetch(`http://127.0.0.1:${port}/healthz`);
      if (res.ok) {
        return;
      }
    } catch {
      // Not listening yet.
    }
    if (Date.now() > until) {
      throw new Error(`kubehenge serve --demo did not answer on port ${port}`);
    }
    await sleep(pollInterval);
  }
}

/** startDemo runs the binary's demo on 127.0.0.1:port and returns once it answers. Kill the process to stop it. */
export async function startDemo(binary: string, port: string): Promise<ChildProcess> {
  const server = spawn(binary, ["serve", "--demo", "--demo-time", demoTime, "--listen", `127.0.0.1:${port}`], {
    stdio: ["ignore", "ignore", "inherit"],
  });
  try {
    await waitForServer(server, port);
  } catch (err) {
    server.kill();
    throw err;
  }
  return server;
}
