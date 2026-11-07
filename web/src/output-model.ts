import { oneOf, records, text, texts } from "./values";

export const outputKinds = ["syslog", "otlp", "loki"] as const;
export type OutputKind = (typeof outputKinds)[number];

export const outputStreams = ["containers", "events", "nodes", "audit"] as const;
export type OutputStream = (typeof outputStreams)[number];

export const outputAuths = ["none", "basic", "bearer"] as const;
export type OutputAuth = (typeof outputAuths)[number];

/** A destination every log also goes to, beside the cluster's Loki. */
export interface LogOutput {
  name: string;
  kind: OutputKind;
  endpoint: string;
  /** PEM certificates for a private CA. */
  caBundle: string;
  /** Empty sends every stream. */
  streams: OutputStream[];
  auth: OutputAuth;
}

export const maxOutputs = 4;
export const maxOutputName = 20;

const namePattern = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/u;

/** newOutput is an empty output, which names and addresses nothing yet. */
export const newOutput = (): LogOutput => ({
  name: "",
  kind: "syslog",
  endpoint: "",
  caBundle: "",
  streams: [],
  auth: "none",
});

export function outputsOf(v: unknown): LogOutput[] {
  return records(v).map((r) => {
    const kind = text(r["kind"]);
    const auth = text(r["auth"]);
    return {
      name: text(r["name"]) ?? "",
      kind: oneOf(outputKinds, kind) ? kind : "syslog",
      endpoint: text(r["endpoint"]) ?? "",
      caBundle: text(r["caBundle"]) ?? "",
      streams: (texts(r["streams"]) ?? []).filter((s): s is OutputStream => oneOf(outputStreams, s)),
      auth: oneOf(outputAuths, auth) ? auth : "none",
    };
  });
}

/** outputVariable is an output as the logs variable holds it, with defaults left out. */
export const outputVariable = (o: LogOutput): Record<string, unknown> => ({
  name: o.name,
  kind: o.kind,
  endpoint: o.endpoint,
  ...(o.caBundle === "" ? {} : { caBundle: o.caBundle }),
  ...(o.streams.length === 0 ? {} : { streams: o.streams }),
  ...(o.auth === "none" ? {} : { auth: o.auth }),
});

/** The credential purpose of an output, as the server names it. */
export const outputPurpose = (name: string): string => `log-${name}`;

const syslogSchemes = ["tcp://", "tls://", "udp://"];

function endpointProblem(o: LogOutput): string | undefined {
  if (o.endpoint === "") {
    return "Enter an endpoint";
  }
  let url: URL;
  try {
    url = new URL(o.endpoint);
  } catch {
    return "Not a URL";
  }
  if (url.username !== "" || url.password !== "" || url.search !== "" || url.hash !== "") {
    return "No user, query or fragment. Credentials are stored apart.";
  }
  if (o.kind === "syslog") {
    if (!syslogSchemes.some((s) => o.endpoint.startsWith(s))) {
      return "Start with tcp://, tls:// or udp://";
    }
    return url.port === "" ? "Add a port" : undefined;
  }
  return /^https?:\/\//u.test(o.endpoint) ? undefined : "Start with https:// or http://";
}

const encrypted = (o: LogOutput): boolean => o.endpoint.startsWith("https://") || o.endpoint.startsWith("tls://");

/**
 * outputProblems says what is wrong with each output, keyed `<index>.<field>`. It mirrors the
 * webhook, which refuses the same.
 */
export function outputProblems(outputs: LogOutput[]): Record<string, string> {
  const p: Record<string, string> = {};
  if (outputs.length > maxOutputs) {
    p["count"] = `At most ${maxOutputs} outputs`;
  }
  outputs.forEach((o, i) => {
    if (!namePattern.test(o.name) || o.name.length > maxOutputName) {
      p[`${i}.name`] = `Lower-case letters, digits and hyphens, at most ${maxOutputName}`;
    } else if (outputs.some((other, j) => j < i && other.name === o.name)) {
      p[`${i}.name`] = "Already used";
    }
    const endpoint = endpointProblem(o);
    if (endpoint !== undefined) {
      p[`${i}.endpoint`] = endpoint;
    }
    if (o.caBundle !== "" && !encrypted(o)) {
      p[`${i}.caBundle`] = "Needs an https:// or tls:// endpoint";
    } else if (o.caBundle !== "" && !o.caBundle.includes("-----BEGIN CERTIFICATE-----")) {
      p[`${i}.caBundle`] = "Paste PEM certificates";
    }
    if (o.auth !== "none" && !o.endpoint.startsWith("https://")) {
      p[`${i}.auth`] = "Credentials go only to https://";
    }
  });
  return p;
}

/** outputsProblem is the first problem of a set of outputs, for a step that holds them. */
export const outputsProblem = (outputs: LogOutput[]): string | undefined => Object.values(outputProblems(outputs))[0];
