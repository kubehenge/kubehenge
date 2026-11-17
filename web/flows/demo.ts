// What the screenshots and the interface's flow tests share: `kubehenge serve --demo` started on a
// free loopback port, the demo's clock and the browser set-up that reads it the same way.
import { type ChildProcess, spawn } from "node:child_process";
import { createServer } from "node:net";

// The demo's clock, which the browser's is set to as well, so that ages read the same in both.
export const demoTime = "2026-10-13T09:00:00Z";
export const viewport = { width: 1440, height: 900 };
// The server listens on 127.0.0.1 only. The browser reaches it as localhost, which it treats as a
// secure origin, so that the session cookie (marked Secure) is kept over plain HTTP.
export const hostResolverRule = "--host-resolver-rules=MAP localhost 127.0.0.1";

const startDeadline = 30_000;

/** A running demo: the process, and the origin the browser opens. */
export interface Demo {
  server: ChildProcess;
  origin: string;
}

// A port the system hands out, which nothing else holds until a process binds it.
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = createServer();
    probe.once("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const address = probe.address();
      probe.close(() => {
        if (address === null || typeof address === "string") {
          reject(new Error("no port was assigned"));
        } else {
          resolve(address.port);
        }
      });
    });
  });
}

// Resolves once this process has logged that it serves on the address, which only a process that
// bound the port does. Another run's server on the same port could answer the health check alone.
function serving(server: ChildProcess, address: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`kubehenge serve --demo did not report serving on ${address}`));
    }, startDeadline);
    let seen = "";
    server.stderr?.on("data", (chunk: Buffer) => {
      process.stderr.write(chunk);
      seen += chunk.toString();
      if (seen.includes(`"msg":"serving","address":"${address}"`)) {
        clearTimeout(timer);
        resolve();
      }
    });
    server.once("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`kubehenge serve --demo exited with ${code}`));
    });
  });
}

/** startDemo runs the binary's demo on a free 127.0.0.1 port and returns once it serves and answers. Kill the process to stop it. */
export async function startDemo(binary: string): Promise<Demo> {
  const port = await freePort();
  const address = `127.0.0.1:${port}`;
  const server = spawn(binary, ["serve", "--demo", "--demo-time", demoTime, "--listen", address], {
    stdio: ["ignore", "ignore", "pipe"],
  });
  try {
    await serving(server, address);
    const res = await fetch(`http://${address}/healthz`);
    if (!res.ok || server.exitCode !== null) {
      throw new Error(`kubehenge serve --demo answered ${res.status} on ${address}`);
    }
  } catch (err) {
    server.kill();
    throw err;
  }
  return { server, origin: `http://localhost:${port}` };
}
