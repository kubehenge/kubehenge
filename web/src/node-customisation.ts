import { oneOf, record, records, text } from "./values";

/** The permissions a file may have, as the cluster variable lists them. */
export const filePermissions = ["0400", "0440", "0444", "0600", "0640", "0644", "0700", "0750", "0755"] as const;

/** Which nodes get a file or a command: both kinds, or one. */
export const nodeRoles = ["both", "controlPlane", "worker"] as const;
export type NodeRoles = (typeof nodeRoles)[number];

export const roleNames: Record<NodeRoles, string> = {
  both: "All nodes",
  controlPlane: "Control plane",
  worker: "Workers",
};

export const maxFiles = 10;
export const maxCommands = 10;
export const maxArgs = 20;
export const maxCommandLength = 1024;

const maxName = 253;

/** A file as it is edited: its text inline, or the key of a Secret. */
export interface NodeFile {
  path: string;
  permissions: string;
  source: "content" | "secret";
  content: string;
  secretName: string;
  secretKey: string;
  roles: NodeRoles;
}

export interface NodeCommand {
  run: string;
  roles: NodeRoles;
}

export interface NodeArg {
  name: string;
  value: string;
}

/** The lists of flags, by the cluster variable's field for each component. */
export const argLists = ["kubeletArgs", "apiServerArgs", "controllerManagerArgs", "schedulerArgs"] as const;
export type ArgList = (typeof argLists)[number];

export const argListNames: Record<ArgList, string> = {
  kubeletArgs: "Kubelet flags",
  apiServerArgs: "API server flags",
  controllerManagerArgs: "Controller manager flags",
  schedulerArgs: "Scheduler flags",
};

/** What the nodeCustomisation variable holds, as the form edits it. */
export interface NodeCustomisation {
  files: NodeFile[];
  commands: NodeCommand[];
  kubeletArgs: NodeArg[];
  apiServerArgs: NodeArg[];
  controllerManagerArgs: NodeArg[];
  schedulerArgs: NodeArg[];
  /** The ConfigMap of the audit policy, named. */
  auditConfigMap: string;
  /** The Secret of the audit webhook's kubeconfig, and its key. */
  webhookSecret: string;
  webhookKey: string;
  /** An inline audit policy that kubectl set. The interface keeps it as it is. */
  inlinePolicy: unknown;
}

export const noNodeCustomisation: NodeCustomisation = {
  files: [],
  commands: [],
  kubeletArgs: [],
  apiServerArgs: [],
  controllerManagerArgs: [],
  schedulerArgs: [],
  auditConfigMap: "",
  webhookSecret: "",
  webhookKey: "",
  inlinePolicy: undefined,
};

export const newFile = (): NodeFile => ({
  path: "",
  permissions: "0644",
  source: "content",
  content: "",
  secretName: "",
  secretKey: "",
  roles: "both",
});

const dnsName = /^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/u;
const keyName = /^[-._a-zA-Z0-9]+$/u;
const flagName = /^[a-z][a-z0-9-]*$/u;

/** pathProblem says what is wrong with a file's path, or undefined where it holds. */
export function pathProblem(path: string): string | undefined {
  if (path === "") {
    return "Enter a path";
  }
  if (!path.startsWith("/")) {
    return "Start the path with /";
  }
  if (path.split("/").includes("..")) {
    return "The path cannot contain ..";
  }
  return path.endsWith("/") ? "Name a file, not a folder" : undefined;
}

/** secretNameProblem says what is wrong with the name of a Secret or ConfigMap. */
export function objectNameProblem(name: string): string | undefined {
  if (name === "") {
    return "Enter a name";
  }
  return name.length > maxName || !dnsName.test(name) ? "Use lower-case letters, digits, dashes and dots" : undefined;
}

/** secretKeyProblem says what is wrong with a key inside a Secret. */
export function secretKeyProblem(key: string): string | undefined {
  if (key === "") {
    return "Enter a key";
  }
  return key.length > maxName || !keyName.test(key) ? "Use letters, digits, dashes, dots and underscores" : undefined;
}

/** fileProblem says what keeps a file from being sent. */
export function fileProblem(f: NodeFile): string | undefined {
  const path = pathProblem(f.path);
  if (path !== undefined) {
    return path;
  }
  if (f.source === "content") {
    return f.content === "" ? "Enter the file's text" : undefined;
  }
  const name = objectNameProblem(f.secretName);
  return name === undefined ? secretKeyProblem(f.secretKey) : `Secret: ${name}`;
}

/** commandProblem says what keeps a command from being sent. */
export function commandProblem(c: NodeCommand): string | undefined {
  if (c.run.trim() === "") {
    return "Enter a command";
  }
  return c.run.length > maxCommandLength ? `At most ${maxCommandLength} characters` : undefined;
}

/** argProblems lists each flag's problem, in the flags' order. */
export function argProblems(args: NodeArg[]): (string | undefined)[] {
  return args.map((a, i) => {
    if (a.name === "") {
      return "Enter a flag name";
    }
    if (!flagName.test(a.name)) {
      return "Name the flag without dashes, such as event-qps";
    }
    return args.findIndex((o) => o.name === a.name) === i ? undefined : "Each flag once";
  });
}

/** What a distribution cannot carry, which the form hides and the request leaves out. */
export interface Carries {
  commands: boolean;
  secrets: boolean;
  auditConfigMap: boolean;
  webhook: boolean;
}

/** carries says what a distribution takes: Talos reads neither a Secret nor a ConfigMap and runs no commands. */
export const carries = (talos: boolean): Carries => ({
  commands: !talos,
  secrets: !talos,
  auditConfigMap: !talos,
  webhook: !talos,
});

const first = (list: (string | undefined)[]): string | undefined => list.find((p) => p !== undefined);

function countProblem(v: NodeCustomisation): string | undefined {
  if (v.files.length > maxFiles) {
    return `At most ${maxFiles} files`;
  }
  if (v.commands.length > maxCommands) {
    return `At most ${maxCommands} commands`;
  }
  return argLists.some((l) => v[l].length > maxArgs) ? `At most ${maxArgs} flags a list` : undefined;
}

function auditProblem(v: NodeCustomisation, can: Carries): string | undefined {
  if (can.auditConfigMap && v.auditConfigMap !== "") {
    return objectNameProblem(v.auditConfigMap);
  }
  if (can.webhook && v.webhookSecret !== "") {
    return objectNameProblem(v.webhookSecret) ?? (v.webhookKey === "" ? undefined : secretKeyProblem(v.webhookKey));
  }
  return undefined;
}

/** nodeCustomisationProblem says what keeps the settings from being sent: the counts, then the first row's. */
export function nodeCustomisationProblem(v: NodeCustomisation, talos: boolean): string | undefined {
  const can = carries(talos);
  return (
    countProblem(v) ??
    first(
      v.files.map((f) => (!can.secrets && f.source === "secret" ? "Talos takes inline text only" : fileProblem(f))),
    ) ??
    first(can.commands ? v.commands.map(commandProblem) : []) ??
    first(argLists.flatMap((l) => argProblems(v[l]))) ??
    auditProblem(v, can)
  );
}

const rolesOf = (v: unknown): NodeRoles => {
  const list = Array.isArray(v) ? v : [];
  const control = list.includes("controlPlane");
  const worker = list.includes("worker");
  if (control === worker) {
    return "both";
  }
  return control ? "controlPlane" : "worker";
};

const rolesVariable = (r: NodeRoles): { roles?: string[] } => (r === "both" ? {} : { roles: [r] });

const argsOf = (v: unknown): NodeArg[] =>
  records(v).map((a) => ({ name: text(a["name"]) ?? "", value: text(a["value"]) ?? "" }));

function fileOf(f: Record<string, unknown>): NodeFile {
  const secret = record(f["secret"]);
  const hasSecret = text(secret["name"]) !== undefined;
  const permissions = text(f["permissions"]);
  return {
    path: text(f["path"]) ?? "",
    permissions: oneOf(filePermissions, permissions) ? permissions : "0644",
    source: hasSecret ? "secret" : "content",
    content: text(f["content"]) ?? "",
    secretName: text(secret["name"]) ?? "",
    secretKey: text(secret["key"]) ?? "",
    roles: rolesOf(f["roles"]),
  };
}

/** nodeCustomisationOf reads a nodeCustomisation variable. */
export function nodeCustomisationOf(v: unknown): NodeCustomisation {
  const r = record(v);
  const webhook = record(r["auditWebhook"]);
  const policy = record(r["auditPolicy"]);
  return {
    files: records(r["files"]).map((f) => fileOf(f)),
    commands: records(r["commands"]).map((c) => ({ run: text(c["run"]) ?? "", roles: rolesOf(c["roles"]) })),
    kubeletArgs: argsOf(r["kubeletArgs"]),
    apiServerArgs: argsOf(r["apiServerArgs"]),
    controllerManagerArgs: argsOf(r["controllerManagerArgs"]),
    schedulerArgs: argsOf(r["schedulerArgs"]),
    auditConfigMap: text(policy["configMap"]) ?? "",
    webhookSecret: text(webhook["secret"]) ?? "",
    webhookKey: text(webhook["key"]) ?? "",
    inlinePolicy: policy["policy"],
  };
}

function fileVariable(f: NodeFile): Record<string, unknown> {
  return {
    path: f.path,
    permissions: f.permissions,
    ...(f.source === "content" ? { content: f.content } : { secret: { name: f.secretName, key: f.secretKey } }),
    ...rolesVariable(f.roles),
  };
}

/**
 * nodeCustomisationVariable is the settings as the API takes them, with what the distribution cannot
 * carry left out. An empty variable is {}, which a change sends to clear it.
 */
export function nodeCustomisationVariable(v: NodeCustomisation, talos: boolean): Record<string, unknown> {
  const can = carries(talos);
  const out: Record<string, unknown> = {};
  const files = v.files.filter((f) => can.secrets || f.source === "content");
  if (files.length > 0) {
    out["files"] = files.map((f) => fileVariable(f));
  }
  if (can.commands && v.commands.length > 0) {
    out["commands"] = v.commands.map((c) => ({ run: c.run, ...rolesVariable(c.roles) }));
  }
  for (const list of argLists) {
    if (v[list].length > 0) {
      out[list] = v[list].map((a) => ({ name: a.name, value: a.value }));
    }
  }
  if (v.inlinePolicy !== undefined) {
    out["auditPolicy"] = { policy: v.inlinePolicy };
  } else if (can.auditConfigMap && v.auditConfigMap !== "") {
    out["auditPolicy"] = { configMap: v.auditConfigMap };
  }
  if (can.webhook && v.webhookSecret !== "") {
    out["auditWebhook"] = { secret: v.webhookSecret, ...(v.webhookKey !== "" && { key: v.webhookKey }) };
  }
  return out;
}

/** hasNodeCustomisation tells whether the variable would send anything. */
export const hasNodeCustomisation = (v: NodeCustomisation, talos: boolean): boolean =>
  Object.keys(nodeCustomisationVariable(v, talos)).length > 0;

export const sameNodeCustomisation = (a: NodeCustomisation, b: NodeCustomisation, talos: boolean): boolean =>
  JSON.stringify(nodeCustomisationVariable(a, talos)) === JSON.stringify(nodeCustomisationVariable(b, talos));

const plural = (n: number, one: string, many: string): string => `${n} ${n === 1 ? one : many}`;

/** nodeCustomisationText sums the settings up in a line, for a summary. */
export function nodeCustomisationText(v: NodeCustomisation): string {
  const flags = argLists.reduce((n, l) => n + v[l].length, 0);
  const audit = v.auditConfigMap !== "" || v.inlinePolicy !== undefined || v.webhookSecret !== "";
  const parts = [
    ...(v.files.length > 0 ? [plural(v.files.length, "file", "files")] : []),
    ...(v.commands.length > 0 ? [plural(v.commands.length, "command", "commands")] : []),
    ...(flags > 0 ? [plural(flags, "flag", "flags")] : []),
    ...(audit ? ["audit"] : []),
  ];
  return parts.length > 0 ? parts.join(", ") : "None";
}

// A short, stable tag of a text, so that an edit of a file's content shows in a plan without the text.
function tag(s: string): string {
  let h = fnvOffset;
  for (const ch of s) {
    h = Math.imul(h ^ (ch.codePointAt(0) ?? 0), fnvPrime);
  }
  return (h >>> 0).toString(hexRadix).padStart(tagWidth, "0");
}

// The 32-bit FNV-1a offset and prime.
const fnvOffset = 2_166_136_261;
const fnvPrime = 16_777_619;
const hexRadix = 16;
const tagWidth = 8;

const flagKinds: Record<ArgList, string> = {
  kubeletArgs: "Kubelet flag",
  apiServerArgs: "API server flag",
  controllerManagerArgs: "Controller manager flag",
  schedulerArgs: "Scheduler flag",
};

/** Each item of a customisation by what identifies it, with the line that says what it holds. */
function nodeItems(v: NodeCustomisation): Map<string, string> {
  const items = new Map<string, string>();
  for (const f of v.files) {
    const from = f.source === "secret" ? `Secret ${f.secretName}/${f.secretKey}` : `text #${tag(f.content)}`;
    items.set(`file ${f.path}`, `File ${f.path}: ${from}, ${f.permissions}, ${roleNames[f.roles].toLowerCase()}`);
  }
  for (const c of v.commands) {
    items.set(`command ${c.run}`, `Command ${c.run}: ${roleNames[c.roles].toLowerCase()}`);
  }
  for (const list of argLists) {
    for (const a of v[list]) {
      items.set(`${list} ${a.name}`, `${flagKinds[list]} ${a.name}=${a.value}`);
    }
  }
  if (v.auditConfigMap !== "") {
    items.set("policy", `Audit policy: ConfigMap ${v.auditConfigMap}`);
  } else if (v.inlinePolicy !== undefined) {
    items.set("policy", `Audit policy: text #${tag(JSON.stringify(v.inlinePolicy))}`);
  }
  if (v.webhookSecret !== "") {
    items.set(
      "webhook",
      `Audit webhook: Secret ${v.webhookSecret}/${v.webhookKey === "" ? "kubeconfig" : v.webhookKey}`,
    );
  }
  return items;
}

/**
 * nodeCustomisationChanges says, one line per item, what a change of the customisation adds,
 * removes or alters. The two lists are as long as each other, so that line n of one pairs with
 * line n of the other, and a missing side reads as a dash.
 */
export function nodeCustomisationChanges(
  was: NodeCustomisation,
  now: NodeCustomisation,
): { was: string[]; now: string[] } {
  const before = nodeItems(was);
  const after = nodeItems(now);
  const out: { was: string[]; now: string[] } = { was: [], now: [] };
  for (const [key, line] of before) {
    if (after.get(key) !== line) {
      out.was.push(line);
      out.now.push(after.get(key) ?? "–");
    }
  }
  for (const [key, line] of after) {
    if (!before.has(key)) {
      out.was.push("–");
      out.now.push(line);
    }
  }
  return out;
}
