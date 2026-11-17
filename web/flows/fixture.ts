// The flow tests' shared set-up: one `kubehenge serve --demo` for the run, a browser clock fixed to
// the demo's, and a log of what each page did wrong.
import { resolve } from "node:path";

import { test as base, expect, type Page, type Request } from "@playwright/test";

import { demoOrigin, demoTime, startDemo } from "./demo.ts";

export { expect } from "@playwright/test";

/** refusal is what the demo answers to every change, as the page shows it. */
export const refusal = "The demo changes nothing. Install Kubehenge to manage clusters.";

/** Problems is what a page logged that the test expected: `allow` names one as `<status> <method> <path>`. */
interface Problems {
  allow: (entry: string) => void;
}

const binary = process.env["KUBEHENGE_BIN"];
const port = process.env["FLOWS_PORT"] ?? "18444";
const firstErrorStatus = 400;

const pathOf = (r: Request): string => new URL(r.url()).pathname;

export const test = base.extend<{ problems: Problems }, { origin: string }>({
  origin: [
    // eslint-disable-next-line no-empty-pattern -- Playwright reads the fixtures a function takes from its first parameter.
    async ({}, use) => {
      if (binary === undefined) {
        throw new Error("KUBEHENGE_BIN names no binary. `make ui-flows` builds one and runs the tests with it.");
      }
      const server = await startDemo(resolve(binary), port);
      try {
        await use(demoOrigin(port));
      } finally {
        server.kill();
      }
    },
    { scope: "worker" },
  ],
  baseURL: async ({ origin }, use) => {
    await use(origin);
  },
  // Every test asserts that its page logged no console error, page error, failed request or error
  // response beyond the ones it allowed, and that each one it allowed did happen.
  problems: [
    async ({ page }, use) => {
      const seen = new Set<string>();
      const allowed = new Set<string>();
      page.on("console", (m) => {
        // The failed response is logged on its own, with its status and path.
        if (m.type() === "error" && !m.text().startsWith("Failed to load resource")) {
          seen.add(`console error: ${m.text()}`);
        }
      });
      page.on("pageerror", (e) => {
        seen.add(`page error: ${e.message}`);
      });
      page.on("requestfailed", (r) => {
        const error = r.failure()?.errorText ?? "";
        // The live event stream stays open until the page closes it, on signing out or leaving a view.
        if (pathOf(r) !== "/api/events" || error !== "net::ERR_ABORTED") {
          seen.add(`failed ${r.method()} ${pathOf(r)}: ${error}`);
        }
      });
      page.on("response", (r) => {
        if (r.status() >= firstErrorStatus) {
          seen.add(`${r.status()} ${r.request().method()} ${pathOf(r.request())}`);
        }
      });
      await use({
        allow: (entry) => {
          allowed.add(entry);
        },
      });
      expect(
        [...seen].filter((s) => !allowed.has(s)),
        "what the page logged beyond what the test allowed",
      ).toEqual([]);
      expect(
        [...allowed].filter((a) => !seen.has(a)),
        "what the test allowed that the page never logged",
      ).toEqual([]);
    },
    { auto: true },
  ],
  page: async ({ page }, use) => {
    await page.clock.setFixedTime(new Date(demoTime));
    await use(page);
  },
});

/** sent waits for the page to send a request to the path, and returns it. Start it before the click that sends it. */
export function sent(page: Page, method: string, path: string): Promise<Request> {
  return page.waitForRequest((r) => {
    const url = new URL(r.url());
    return r.method() === method && url.pathname + url.search === path;
  });
}

export const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null;

/** cluster reads a cluster back from the API, as the demo holds it. */
export async function cluster(page: Page, name: string): Promise<Record<string, unknown> | undefined> {
  const res = await page.request.get("/api/clusters");
  expect(res.ok()).toBe(true);
  const body: unknown = await res.json();
  const all: unknown[] = Array.isArray(body) ? body : [];
  const found = all.find((c) => isRecord(c) && c["name"] === name);
  return isRecord(found) ? found : undefined;
}
