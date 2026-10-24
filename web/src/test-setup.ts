// expect.extend takes the matchers as one object, and the module exports them by name only.
// oxlint-disable-next-line import/no-namespace
import * as matchers from "@testing-library/jest-dom/matchers";
import { cleanup, configure } from "@testing-library/react";
import { afterEach, expect, vi } from "vitest";

expect.extend(matchers);

// A page's first render waits on its fake server's answers; with every file at once on a loaded
// machine it took longer than the 1 s Testing Library waits by default. Checks that something stays
// away give their own short timeout.
configure({ asyncUtilTimeout: 10_000 });

// A page's chain of requests can go on after its test ends, once restoreAllMocks has taken the
// fake server away; happy-dom would then send the next one to the network.
globalThis.fetch = (input) =>
  Promise.reject(new Error(`no fake server answers ${input instanceof Request ? input.url : input.toString()}`));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  window.history.replaceState(null, "", "/");
  localStorage.clear();
});
