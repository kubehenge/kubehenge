import { describe, expect, it } from "vitest";

import { check, gather, gatherSource, type Finding, type UiText } from "./writing";

// The rare text that has to break a rule, each with the reason it must. An entry matches the
// findings of its rule in its file whose text starts with its text. One that matches nothing fails
// the test, so the list keeps only what is still needed.
const exceptions: readonly { file: string; rule: string; text: string; reason: string }[] = [
  {
    file: "BackupTargets.tsx",
    rule: "exclamation",
    text: "use letters, digits and !_.*'()-",
    reason: "the ! is one of the characters a bucket folder may hold, not punctuation",
  },
];

type Exception = (typeof exceptions)[number];

const covers = (e: Exception, f: Finding): boolean =>
  e.file === f.file && e.rule === f.rule && f.text.startsWith(e.text);
const excused = (f: Finding): boolean => exceptions.some((e) => covers(e, f));

const where = (f: Finding): string => `src/${f.file}:${f.line} ${f.rule} (${f.detail}): "${f.text}"`;

const texts = gather(import.meta.dirname);
const findings = texts.flatMap((t) => check(t));

const textOf = (source: string): string[] => gatherSource("a.tsx", source).map((t) => `${t.kind}: ${t.text}`);
const rules = (text: string, kind: UiText["kind"] = "text"): string[] =>
  check({ file: "a.tsx", line: 1, kind, text }).map((f) => f.rule);

describe("the interface's words", () => {
  it("keep to web/WRITING.md", () => {
    expect(findings.filter((f) => !excused(f)).map((f) => where(f))).toStrictEqual([]);
  });

  it("need every exception made for them", () => {
    expect(exceptions.filter((e) => !findings.some((f) => covers(e, f)))).toStrictEqual([]);
  });
});

describe("gathering the interface's words", () => {
  it("reads JSX text with its inline elements and values, and each block apart", () => {
    expect(
      textOf(
        'const e = <div><p>Runs on <code>pve1</code> since {when}.</p><button type="button">Back up now</button></div>;',
      ),
    ).toStrictEqual(["text: Runs on pve1 since {x}.", "button: Back up now"]);
  });

  it("reads the attributes people read, by the kind of text they hold", () => {
    expect(
      textOf('const e = <Field label="Time zone" hint="The hour is in this time zone." className="wide field" />;'),
    ).toStrictEqual(["label: Time zone", "hint: The hour is in this time zone."]);
  });

  it("reads strings and templates outside JSX, and leaves code alone", () => {
    expect(
      textOf(
        `const errorText = \`Failed on \${node} twice\`; if (s === "Not a text here") { fetch("/api/a b"); } import x from "./a b"; type T = "Some type";`,
      ),
    ).toStrictEqual(["error: Failed on {x} twice"]);
  });
});

describe("the rules", () => {
  it("refuse dashes, filler, hedges and exclamations", () => {
    expect(rules(`Done ${String.fromCodePoint(0x20_14)} all of it`)).toStrictEqual(["dash"]);
    expect(rules("Simply choose one")).toStrictEqual(["banned-word"]);
    expect(rules("This might take a while")).toStrictEqual(["banned-word"]);
    expect(rules("Saved!")).toStrictEqual(["exclamation"]);
  });

  it("refuse semicolon chains and explanations bolted on with a colon", () => {
    expect(rules("Off; the cluster keeps its nodes")).toStrictEqual(["semicolon"]);
    expect(rules("Failed: the release's checksum does not match the index")).toStrictEqual(["colon-clause"]);
    expect(rules("Failed: checksum mismatch")).toStrictEqual([]);
  });

  it("hold each kind of text to its length", () => {
    expect(rules("Every day at 02:00, kept 7 days", "hint")).toStrictEqual([]);
    expect(rules("Kubehenge's own releases only, once a day at the hour chosen, nothing else", "hint")).toStrictEqual([
      "too-long",
    ]);
    expect(rules("Saved. The interface restarts.", "hint")).toStrictEqual(["sentences"]);
    expect(rules("Upgrade the management cluster now", "button")).toStrictEqual(["too-long"]);
  });

  it("write labels and buttons in sentence case, without a full stop", () => {
    expect(rules("Download Admin Kubeconfig", "button")).toStrictEqual(["title-case"]);
    expect(rules("Proxmox VE node", "label")).toStrictEqual([]);
    expect(rules("Time zone.", "label")).toStrictEqual(["full-stop"]);
  });
});
