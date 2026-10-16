import { vi } from "vitest";

/** What a route answers: a status, 200 unless given, and a JSON body or else a text. */
export interface Answer {
  status?: number;
  body?: unknown;
  text?: string;
}

/** A route answers as given, or works its answer out from the request's JSON body. */
export type Route = Answer | ((body: unknown) => Answer | Promise<Answer>);

/** A request the page made. */
export interface Call {
  key: string;
  body: unknown;
  headers: Headers;
}

const httpOK = 200;
const httpAccepted = 202;
const httpNoContent = 204;
const httpNotFound = 404;

// Statuses whose answer may have no body at all.
const bodiless = new Set([httpAccepted, httpNoContent]);

function pathOf(input: RequestInfo | URL): string {
  if (typeof input === "string") {
    return input;
  }
  return input instanceof URL ? `${input.pathname}${input.search}` : input.url;
}

/**
 * fakeServer answers fetch from routes keyed by "METHOD path" and records the requests made. It
 * reads routes and headers as each request comes, so a test changes what the server says by
 * changing them.
 */
export function fakeServer(routes: Record<string, Route>, headers: Record<string, string> = {}): Call[] {
  const calls: Call[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const key = `${init.method ?? "GET"} ${pathOf(input)}`;
    const body: unknown = typeof init.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push({ key, body, headers: new Headers(init.headers) });
    const route = routes[key] ?? { status: httpNotFound, body: { error: `no route ${key}` } };
    const answer = typeof route === "function" ? await route(body) : route;
    const status = answer.status ?? httpOK;
    const text =
      answer.text ?? (answer.body === undefined && bodiless.has(status) ? null : JSON.stringify(answer.body ?? {}));
    return new Response(text, { status, headers: { "Content-Type": "application/json", ...headers } });
  });
  return calls;
}

/** held is a route whose answer waits until the test fails it, as a request the network loses. */
export function held(): { route: Route; fail: (err: Error) => void } {
  let fail: ((err: Error) => void) | undefined;
  // A promise settled from outside, as Promise.withResolvers makes one from ES2024 on.
  // oxlint-disable-next-line promise/avoid-new -- the only way to it in ES2023
  const answer = new Promise<Answer>((_resolve, reject) => {
    fail = reject;
  });
  return { route: async () => answer, fail: (err) => fail?.(err) };
}

/** reads counts the requests made with a key. */
export const reads = (calls: Call[], key: string): number => calls.filter((c) => c.key === key).length;
