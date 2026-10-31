import { screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import { expect, vi } from "vitest";

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
const httpBadRequest = 400;
const httpNotFound = 404;

// Statuses whose answer may have no body at all.
const bodiless = new Set([httpAccepted, httpNoContent]);

function pathOf(input: RequestInfo | URL): string {
  if (typeof input === "string") {
    return input;
  }
  return input instanceof URL ? `${input.pathname}${input.search}` : input.url;
}

const answerOf = async (route: Route, body: unknown): Promise<Answer> =>
  typeof route === "function" ? route(body) : route;

// abortOf fails once a request's signal aborts, as a browser's fetch does however long the server takes.
function abortOf(signal: AbortSignal | null | undefined): Promise<never> {
  // oxlint-disable-next-line promise/avoid-new -- settled by the signal's event
  return new Promise((_resolve, reject) => {
    signal?.addEventListener("abort", () => reject(new DOMException("The operation was aborted.", "AbortError")), {
      once: true,
    });
  });
}

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null;

// The settings a change names, as the server lists those it plans.
function settingsOf(req: unknown): string[] {
  if (!isRecord(req)) {
    return [];
  }
  const topology = ["version", "controlPlaneReplicas", "workerPools", "pools", "autoscaling"].filter((k) => k in req);
  return [...topology, ...Object.keys(isRecord(req["variables"]) ? req["variables"] : {})];
}

/**
 * planOf answers a cluster's plan as the server would, from the routes a test gives the cluster:
 * refused as its change route refuses, else with the cluster as that route answers it, or as its
 * read does, and every setting the change names, replacing no machine.
 */
async function planOf(routes: Record<string, Route>, path: string, body: unknown): Promise<Answer | undefined> {
  const change = routes[`PATCH ${path}`];
  if (change === undefined) {
    return undefined;
  }
  const answer = await answerOf(change, body);
  if ((answer.status ?? httpOK) >= httpBadRequest) {
    return answer;
  }
  const read = routes[`GET ${path}`];
  const shown = read === undefined ? undefined : await answerOf(read, undefined);
  const cluster = isRecord(answer.body) && "variables" in answer.body ? answer.body : shown?.body;
  return { body: { cluster, changes: settingsOf(body).map((setting) => ({ setting, effect: "none" })) } };
}

const planPath = /^POST (?<path>.+)\/plan$/u;

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
    const plan = planPath.exec(key)?.groups?.["path"];
    const planned = routes[key] === undefined && plan !== undefined ? await planOf(routes, plan, body) : undefined;
    const route = routes[key] ?? planned ?? { status: httpNotFound, body: { error: `no route ${key}` } };
    const answer = await Promise.race([answerOf(route, body), abortOf(init.signal)]);
    const status = answer.status ?? httpOK;
    const text =
      answer.text ?? (answer.body === undefined && bodiless.has(status) ? null : JSON.stringify(answer.body ?? {}));
    return new Response(text, { status, headers: { "Content-Type": "application/json", ...headers } });
  });
  return calls;
}

/**
 * held is a route whose answer waits until the test gives it or fails it, as a slow request or one
 * the network loses.
 */
export function held(): { route: () => Promise<Answer>; answer: (a: Answer) => void; fail: (err: Error) => void } {
  let answer: ((a: Answer) => void) | undefined;
  let fail: ((err: Error) => void) | undefined;
  // A promise settled from outside, as Promise.withResolvers makes one from ES2024 on.
  // oxlint-disable-next-line promise/avoid-new -- the only way to it in ES2023
  const given = new Promise<Answer>((resolve, reject) => {
    answer = resolve;
    fail = reject;
  });
  return { route: async () => given, answer: (a) => answer?.(a), fail: (err) => fail?.(err) };
}

/** applyStaged reviews the plan of what a cluster's page has staged and applies it. */
export async function applyStaged(user: UserEvent): Promise<void> {
  await user.click(await screen.findByRole("button", { name: "Review plan" }));
  await user.click(within(screen.getByRole("dialog", { name: "Review plan" })).getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
}

/** reads counts the requests made with a key. */
export const reads = (calls: Call[], key: string): number => calls.filter((c) => c.key === key).length;
