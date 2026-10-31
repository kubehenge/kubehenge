// writing gathers every string the interface shows from web/src's source, with the kind of text it
// is, and checks each against the rules in web/WRITING.md. writing.test.ts runs it in `make unit`.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { parseSync } from "vite";

export type Kind = "label" | "button" | "hint" | "info" | "status" | "error" | "text";
type Seen = Kind | "skip";

export interface UiText {
  file: string;
  line: number;
  kind: Kind;
  text: string;
}

export interface Finding extends UiText {
  rule: string;
  detail: string;
}

// An expression inside a string or a JSX text: one word, whose own strings are gathered apart.
export const slot = "{x}";

export const limits: Record<Kind, { words: number; sentences: number }> = {
  label: { words: 4, sentences: 1 },
  button: { words: 3, sentences: 1 },
  hint: { words: 10, sentences: 1 },
  info: { words: 24, sentences: 2 },
  status: { words: 6, sentences: 1 },
  error: { words: 16, sentences: 2 },
  text: { words: 20, sentences: 2 },
};

// The most words a colon may bolt onto what comes before it.
const afterColon = 5;

// Words and phrases that pad, hedge, reassure, sell, narrate or speak as "we", matched as whole
// words: the vocabulary that marks generated text, and the clichés of error messages and buttons.
export const banned = [
  "actually",
  "additionally",
  "allows you to",
  "an error occurred",
  "are you sure",
  "at its core",
  "basically",
  "behind the scenes",
  "best-in-class",
  "bolster",
  "click here",
  "comprehensive",
  "conveniently",
  "crucial",
  "currently",
  "cutting-edge",
  "delve",
  "don't worry",
  "easily",
  "effortless",
  "effortlessly",
  "elevate",
  "empower",
  "enables you to",
  "enhance",
  "enhanced",
  "ensure",
  "ensures",
  "ensuring",
  "essentially",
  "everything you need",
  "feel free",
  "furthermore",
  "game-changer",
  "generally",
  "harness",
  "hassle",
  "hassle-free",
  "here you can",
  "importantly",
  "in conclusion",
  "in order to",
  "in summary",
  "intuitive",
  "invaluable",
  "just",
  "keep in mind",
  "kindly",
  "landscape",
  "let's",
  "lets you",
  "leverage",
  "magic",
  "magically",
  "make sure",
  "meticulous",
  "might",
  "moreover",
  "notably",
  "note that",
  "of course",
  "oops",
  "our",
  "out of the box",
  "peace of mind",
  "perhaps",
  "pivotal",
  "please",
  "possibly",
  "powerful",
  "really",
  "rest assured",
  "robust",
  "seamless",
  "seamlessly",
  "simply",
  "something went wrong",
  "sorry",
  "state-of-the-art",
  "streamline",
  "successfully",
  "this page",
  "this section",
  "truly",
  "typically",
  "ultimately",
  "under the hood",
  "unexpected error",
  "unfortunately",
  "unleash",
  "unlock",
  "usually",
  "utilise",
  "utilize",
  "various",
  "very",
  "we",
  "we'll",
  "we're",
  "we've",
  "welcome to",
  "whether you",
  "with ease",
  "worth noting",
  "you can",
  "you'll be able to",
] as const;

// Let's Encrypt is a name, not a suggestion.
const bannedAlternatives = banned.map((w) => (w === "let's" ? "let's(?! encrypt)" : w)).join("|");
const bannedPattern = new RegExp(String.raw`(?<![\p{L}'-])(${bannedAlternatives})(?![\p{L}'-])`, "iu");

// Patterns of padding: `"not only … but"`, `"not just … but"`, Latin abbreviations, "&" for "and".
const padding: readonly [RegExp, string][] = [
  [/\bnot (?:just|only|merely)\b.*\bbut\b/iu, "not only … but"],
  [/\b(?:e\.g|i\.e|etc)\./iu, "Latin abbreviation"],
  [/ & /u, '"&" for "and"'],
];

// Names that keep their capitals inside a sentence-case label.
export const properNouns = new Set([
  "Admission",
  "Akamai",
  "Alloy",
  "AlmaLinux",
  "Amazon",
  "API",
  "Authentik",
  "Calico",
  "Cilium",
  "Cloud",
  "Cluster",
  "Debian",
  "Dex",
  "Edge",
  "Encrypt",
  "Entra",
  "Fedora",
  "Flannel",
  "Flux",
  "Git",
  "Google",
  "Grafana",
  "Headlamp",
  "Helm",
  "Istio",
  "Keycloak",
  "Kubehenge",
  "Kubernetes",
  "Kyverno",
  "Let's",
  "Linux",
  "Loki",
  "Mesh",
  "OpenTofu",
  "Pod",
  "Prometheus",
  "Proxmox",
  "Rancher",
  "Rocky",
  "Route",
  // The Kubernetes object, not the adjective.
  "Secret",
  "Security",
  "Talos",
  "Terraform",
  "Velero",
  "Windows",
]);

// Elements whose text runs on inside the text around them.
const inline = new Set([
  "a",
  "abbr",
  "b",
  "code",
  "em",
  "i",
  "kbd",
  "mark",
  "s",
  "samp",
  "small",
  "strong",
  "sub",
  "sup",
  "time",
  "u",
  "var",
]);
const labels = new Set([
  "caption",
  "dt",
  "figcaption",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "label",
  "legend",
  "option",
  "summary",
  "th",
  "title",
]);

const attributeKinds: Record<string, Seen> = {
  "aria-label": "label",
  about: "label",
  alt: "label",
  caption: "label",
  heading: "label",
  label: "label",
  legend: "label",
  placeholder: "label",
  title: "label",
  description: "hint",
  detail: "hint",
  help: "hint",
  hint: "hint",
  info: "info",
  note: "hint",
  reason: "status",
  text: "status",
  error: "error",
  message: "error",
  action: "button",
  confirm: "button",
  confirmLabel: "button",
  submit: "button",
  submitLabel: "button",
};

// Attributes no one reads: styling, names, values, references and the like.
const unread =
  /^(?:accept|aria-(?:controls|current|describedby|haspopup|labelledby|live|orientation|sort)|autoComplete|className|colSpan|d|data-.*|dir|download|enterKeyHint|fill|form|height|href|htmlFor|id|inputMode|key|kind|lang|list|max|method|min|mode|name|path|pattern|rel|role|rowSpan|scope|size|src|state|step|stroke|style|target|to|tone|type|value|variant|view|viewBox|width|wrap|xmlns)$/u;

// Calls whose string arguments are code, never text: DOM queries, storage keys, string handling.
const codeCalls = new Set([
  "addEventListener",
  "endsWith",
  "fetch",
  "getAttribute",
  "getElementById",
  "getItem",
  "includes",
  "join",
  "localeCompare",
  "matchMedia",
  "padEnd",
  "padStart",
  "querySelector",
  "querySelectorAll",
  "removeEventListener",
  "removeItem",
  "replace",
  "replaceAll",
  "RegExp",
  "setAttribute",
  "setItem",
  "split",
  "startsWith",
  "URL",
  "URLSearchParams",
]);

const comparisons = new Set(["===", "!==", "==", "!=", "in"]);

// The keys under which a node holds types, which no one reads.
const typeKeys = new Set(["typeAnnotation", "returnType", "typeArguments", "typeParameters"]);

interface AstNode {
  readonly type: string;
  readonly start: number;
  readonly [key: string]: unknown;
}

function isNode(v: unknown): v is AstNode {
  return typeof v === "object" && v !== null && "type" in v && typeof v.type === "string";
}

const field = (node: AstNode, key: string): AstNode | undefined => {
  const v = node[key];
  return isNode(v) ? v : undefined;
};

const nodes = (node: AstNode, key: string): AstNode[] => {
  const v = node[key];
  return Array.isArray(v) ? v.filter((n) => isNode(n)) : [];
};

const stringValue = (node: AstNode | undefined): string | undefined =>
  node?.type === "Literal" && typeof node["value"] === "string" ? node["value"] : undefined;

const nameOf = (node: AstNode | undefined): string => {
  if (node === undefined) {
    return "";
  }
  if (node.type === "Identifier" || node.type === "JSXIdentifier") {
    return typeof node["name"] === "string" ? node["name"] : "";
  }
  if (node.type === "MemberExpression" || node.type === "JSXMemberExpression") {
    return nameOf(field(node, "property"));
  }
  return stringValue(node) ?? "";
};

// A variable's, property's or callee's name says what kind of text its strings are.
function kindByName(name: string, fallback: Seen): Seen {
  if (/class/iu.test(name)) {
    return "skip";
  }
  if (/hint|description|help|detail/iu.test(name)) {
    return "hint";
  }
  if (/label|title|heading/iu.test(name)) {
    return "label";
  }
  if (/reason|status/iu.test(name)) {
    return "status";
  }
  if (/error|message|problem/iu.test(name)) {
    return "error";
  }
  return fallback;
}

class Gatherer {
  readonly texts: UiText[] = [];
  private readonly file: string;
  private readonly lineStarts: number[] = [0];

  constructor(file: string, source: string) {
    this.file = file;
    for (let i = source.indexOf("\n"); i !== -1; i = source.indexOf("\n", i + 1)) {
      this.lineStarts.push(i + 1);
    }
  }

  private record(at: number, raw: string, kind: Seen): void {
    if (kind === "skip") {
      return;
    }
    const text = decode(raw).replaceAll(/\s+/gu, " ").trim();
    const bare = text.replaceAll(slot, "");
    // Single tokens are identifiers, paths, units and values. Text has words.
    if (!/\p{L}{2}/u.test(bare) || /^[\w.@/:+#=-]*$/u.test(bare.trim())) {
      return;
    }
    const line = this.lineStarts.findLastIndex((s) => s <= at) + 1;
    this.texts.push({ file: this.file, line, kind, text });
  }

  visit(node: AstNode | undefined, kind: Seen): void {
    if (
      node === undefined ||
      kind === "skip" ||
      node.type === "ImportDeclaration" ||
      node.type === "ExportAllDeclaration"
    ) {
      return;
    }
    switch (node.type) {
      case "Literal": {
        const s = stringValue(node);
        if (s !== undefined) {
          this.record(node.start, s, kind);
        }
        break;
      }
      case "TemplateLiteral": {
        this.record(node.start, this.template(node, kind), kind);
        break;
      }
      case "JSXElement": {
        this.element(node);
        break;
      }
      case "JSXFragment": {
        this.block(node, kind);
        break;
      }
      case "ExportNamedDeclaration": {
        this.visit(field(node, "declaration"), kind);
        break;
      }
      default: {
        this.compound(node, kind);
      }
    }
  }

  private compound(node: AstNode, kind: Seen): void {
    switch (node.type) {
      case "Property": {
        const key = field(node, "key");
        if (node["computed"] === true) {
          this.visit(key, kind);
        }
        this.visit(field(node, "value"), kindByName(nameOf(key), kind));
        break;
      }
      case "VariableDeclarator": {
        const id = nameOf(field(node, "id"));
        this.visit(field(node, "init"), kindByName(id, kind));
        break;
      }
      case "BinaryExpression": {
        const op = node["operator"];
        for (const side of [field(node, "left"), field(node, "right")]) {
          if (!(typeof op === "string" && comparisons.has(op) && stringValue(side) !== undefined)) {
            this.visit(side, kind);
          }
        }
        break;
      }
      case "SwitchCase": {
        nodes(node, "consequent").forEach((n) => this.visit(n, kind));
        break;
      }
      case "MemberExpression": {
        this.visit(field(node, "object"), kind);
        if (node["computed"] === true && stringValue(field(node, "property")) === undefined) {
          this.visit(field(node, "property"), kind);
        }
        break;
      }
      case "CallExpression":
      case "NewExpression": {
        this.call(node, kind);
        break;
      }
      default: {
        this.children(node, kind);
      }
    }
  }

  private call(node: AstNode, kind: Seen): void {
    const callee = field(node, "callee");
    this.visit(callee, kind);
    const name = nameOf(callee);
    if (codeCalls.has(name)) {
      return;
    }
    const own = name === "Error" ? "error" : kindByName(name.replace(/^set/u, ""), kind);
    nodes(node, "arguments").forEach((n) => this.visit(n, own));
  }

  private children(node: AstNode, kind: Seen): void {
    // Types say nothing to users. The expressions inside an assertion do.
    if (node.type.startsWith("TS")) {
      if (/^TS(?:As|Satisfies|NonNull|Instantiation)Expression$/u.test(node.type)) {
        this.visit(field(node, "expression"), kind);
      }
      return;
    }
    for (const [, v] of Object.entries(node).filter(([k]) => !typeKeys.has(k))) {
      if (Array.isArray(v)) {
        v.filter((n) => isNode(n)).forEach((n) => this.visit(n, kind));
      } else if (isNode(v)) {
        this.visit(v, kind);
      }
    }
  }

  private template(node: AstNode, kind: Seen): string {
    nodes(node, "expressions").forEach((n) => this.visit(n, kind));
    return nodes(node, "quasis")
      .map((q) => {
        const v = q["value"];
        return typeof v === "object" && v !== null && "cooked" in v && typeof v.cooked === "string" ? v.cooked : "";
      })
      .join(slot);
  }

  private element(node: AstNode): void {
    const opening = field(node, "openingElement");
    const name = nameOf(field(opening ?? node, "name"));
    this.attributes(opening, name);
    this.block(node, elementKind(name, opening));
  }

  private attributes(opening: AstNode | undefined, element: string): void {
    const attrs = opening === undefined ? [] : nodes(opening, "attributes");
    for (const attr of attrs.filter((a) => a.type === "JSXAttribute")) {
      const name = nameOf(field(attr, "name"));
      let kind: Seen = attributeKinds[name] ?? (unread.test(name) ? "skip" : "text");
      // A select's options hold their text as children. Their value is data.
      if (element === "option" && name === "value") {
        kind = "skip";
      }
      const value = field(attr, "value");
      if (value?.type === "JSXExpressionContainer") {
        this.visit(field(value, "expression"), kind);
      } else {
        this.visit(value, kind);
      }
    }
  }

  // block records a JSX element's text, its inline children run in with it, each block child apart.
  private block(node: AstNode, kind: Kind): void {
    for (const part of this.flatten(node, kind).split("\n")) {
      this.record(node.start, part, kind);
    }
  }

  private flatten(node: AstNode, kind: Kind): string {
    return nodes(node, "children")
      .map((child) => {
        switch (child.type) {
          case "JSXText": {
            return jsxText(typeof child["value"] === "string" ? child["value"] : "");
          }
          case "JSXExpressionContainer": {
            return this.expression(field(child, "expression"), kind);
          }
          case "JSXFragment": {
            return this.flatten(child, kind);
          }
          case "JSXElement": {
            const opening = field(child, "openingElement");
            const name = nameOf(field(opening ?? child, "name"));
            if (!inline.has(name)) {
              this.element(child);
              return "\n";
            }
            this.attributes(opening, name);
            return this.flatten(child, kind);
          }
          default: {
            return "";
          }
        }
      })
      .join("");
  }

  private expression(expr: AstNode | undefined, kind: Kind): string {
    if (expr === undefined || expr.type === "JSXEmptyExpression") {
      return "";
    }
    const s = stringValue(expr);
    if (s !== undefined) {
      return s;
    }
    if (expr.type === "TemplateLiteral") {
      return this.template(expr, kind);
    }
    // An element or a list of them stands on its own. Anything else is a value in the text.
    if (expr.type === "JSXElement" || expr.type === "JSXFragment") {
      this.visit(expr, kind);
      return "\n";
    }
    this.visit(expr, kind);
    return slot;
  }
}

// elementKind tells what kind of text an element holds, by its name or else its class: a hint under
// a field or choice, a field's error.
function elementKind(name: string, opening: AstNode | undefined): Kind {
  if (name === "button") {
    return "button";
  }
  if (name === "Info") {
    return "info";
  }
  if (labels.has(name)) {
    return "label";
  }
  const className = (opening === undefined ? [] : nodes(opening, "attributes")).find(
    (a) => nameOf(field(a, "name")) === "className",
  );
  const classes = stringValue(className === undefined ? undefined : field(className, "value")) ?? "";
  if (/\binfo-text\b/u.test(classes)) {
    return "info";
  }
  if (/\bhint\b/u.test(classes)) {
    return "hint";
  }
  if (/\b(?:field-)?error\b/u.test(classes)) {
    return "error";
  }
  return "text";
}

const entities: Record<string, string> = { amp: "&", apos: "'", gt: ">", lt: "<", nbsp: " ", quot: '"' };

// decode reads the character references JSX text and attributes may hold.
const decode = (s: string): string => s.replaceAll(/&(?<name>\w+);/gu, (m, name: string) => entities[name] ?? m);

// jsxText follows React: lines are trimmed and joined with a space, blank ones dropped.
function jsxText(value: string): string {
  if (!value.includes("\n")) {
    return value;
  }
  return value
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .join(" ");
}

// gather returns every user-visible string of the source files in dir, test files aside.
export function gather(dir: string): UiText[] {
  const files = readdirSync(dir)
    .filter((f) => /\.tsx?$/u.test(f) && !/\.test\.tsx?$|\.d\.ts$/u.test(f))
    .filter((f) => !["fake-server.ts", "test-setup.ts", "writing.ts"].includes(f))
    .toSorted();
  return files.flatMap((file) => gatherSource(file, readFileSync(join(dir, file), "utf8")));
}

/** gatherSource returns the user-visible strings of one source file, named file. */
export function gatherSource(file: string, source: string): UiText[] {
  const parsed = parseSync(file, source);
  if (parsed.errors.length > 0) {
    throw new Error(`${file} does not parse`);
  }
  const program: unknown = parsed.program;
  if (!isNode(program)) {
    throw new Error(`${file} parses to no program`);
  }
  const g = new Gatherer(file, source);
  g.visit(program, "text");
  return g.texts;
}

export const words = (text: string): number => text.split(/\s+/u).filter((w) => /[\p{L}\p{N}{]/u.test(w)).length;

// sentences counts full stops, question and exclamation marks that end a sentence before another.
export function sentences(text: string): number {
  const ends = text.replaceAll(/\b(?:e\.g|i\.e|etc|vs)\./gu, "").match(/[.?!](?=\s+\S)/gu);
  return (ends?.length ?? 0) + 1;
}

function titleCase(text: string): string | undefined {
  return text
    .split(/[\s/]+/u)
    .slice(1)
    .map((w) => w.replaceAll(/^[("']+|[),.:'"]+$/gu, "").replace(/'s$/u, ""))
    .find((w) => /^\p{Lu}\p{Ll}+$/u.test(w) && !properNouns.has(w));
}

// check returns the rules a text breaks, each with what it found.
export function check(t: UiText): Finding[] {
  const found: [string, string][] = [];
  const { text, kind } = t;
  const dash = /[\u{2013}\u{2014}]| - /u.exec(text);
  if (dash) {
    found.push(["dash", `"${dash[0].trim()}"`]);
  }
  const word = bannedPattern.exec(text);
  if (word) {
    found.push(["banned-word", `"${word[1] ?? ""}"`]);
  }
  const pattern = padding.find(([p]) => p.test(text));
  if (pattern) {
    found.push(["padding", pattern[1]]);
  }
  if (text.includes("!")) {
    found.push(["exclamation", ""]);
  }
  if (text.includes(";")) {
    found.push(["semicolon", ""]);
  }
  const colon = text.lastIndexOf(": ");
  if (colon !== -1 && words(text.slice(colon + 2)) > afterColon) {
    found.push(["colon-clause", `${words(text.slice(colon + 2))} words after the colon, at most ${afterColon}`]);
  }
  const n = words(text);
  if (n > limits[kind].words) {
    found.push(["too-long", `${n} words, a ${kind} has at most ${limits[kind].words}`]);
  }
  const s = sentences(text);
  if (s > limits[kind].sentences) {
    found.push(["sentences", `${s} sentences, a ${kind} has at most ${limits[kind].sentences}`]);
  }
  if (kind === "label" || kind === "button") {
    const capital = titleCase(text);
    if (capital !== undefined) {
      found.push(["title-case", `"${capital}"`]);
    }
    if (text.endsWith(".")) {
      found.push(["full-stop", `a ${kind} ends without one`]);
    }
  }
  return found.map(([rule, detail]) => ({ file: t.file, line: t.line, kind, text, rule, detail }));
}
