// The layout check of a certificate bundle, as the server's certificateBlocks
// (pkg/provisioner/nodesite.go) makes it. testdata/pem-bundles.json holds both to the same
// verdicts. The server also parses each certificate and checks its dates.

const begin = "-----BEGIN ";
const end = "-----END ";
const dashes = "-----";

// labelLine finds a BEGIN or END with an upper-case label, as left of a boundary that lost its
// dashes or some of them. A comment such as "# BEGIN of the chain" has no label.
const labelLine = /\b(?:BEGIN|END) [A-Z][A-Z0-9 ]*[A-Z0-9](?:-|$)/u;
// bodyLine is a line of base64 as long as most of a block's lines.
const bodyLine = /^[A-Za-z0-9+/=]{40,}$/u;
const base64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u;
// The server's limit of a cluster's bundles. The appliance's proxy and chart mirror authorities
// take 256.
const nodeKiB = 64;
// space is what the server trims (Go's unicode.IsSpace), which differs from String.prototype.trim:
// it takes U+0085 and leaves U+FEFF.
const space = String.raw`\t\n\v\f\r \x85\xA0\u{1680}\u{2000}-\u{200A}\u{2028}\u{2029}\u{202F}\u{205F}\u{3000}`;
const kibibyte = 1024;
const outerSpace = new RegExp(`^[${space}]+|[${space}]+$`, "gu");
const trim = (s: string): string => s.replaceAll(outerSpace, "");

interface NumberedLine {
  n: number;
  text: string;
}

// lines splits a bundle into its lines, a block that starts on the line another ends on included.
function lines(bundle: string): NumberedLine[] {
  const out: NumberedLine[] = [];
  for (const [i, raw] of bundle.split("\n").entries()) {
    let line = raw.replace(/\r+$/u, "");
    for (let j = line.indexOf(begin, 1); j > 0; j = line.indexOf(begin, 1)) {
      out.push({ n: i + 1, text: line.slice(0, j) });
      line = line.slice(j);
    }
    out.push({ n: i + 1, text: line });
  }
  return out;
}

const trimEnd = (s: string): string => s.replace(/[ \t]+$/u, "");

// blockType is the type of a block from its BEGIN line to its END line, undefined when it is no
// complete PEM block.
function blockType(block: string[]): string | undefined {
  const head = trimEnd(block[0] ?? "");
  const tail = trimEnd(block.at(-1) ?? "");
  if (!head.endsWith(dashes) || head.length < begin.length + dashes.length) {
    return undefined;
  }
  const type = head.slice(begin.length, -dashes.length);
  let body = block.slice(1, -1);
  while (body[0]?.includes(":") === true) {
    body = body.slice(1);
  }
  // The server's decoder passes over carriage returns as well.
  const data = body.join("").replaceAll(/[ \t\r]/gu, "");
  return tail === `${end}${type}${dashes}` && base64.test(data) ? type : undefined;
}

// outsideProblem is what is wrong with line n, which lies outside the blocks.
function outsideProblem(text: string, n: number): string | undefined {
  const line = trim(text);
  if (labelLine.test(line) || (line.includes("--") && line.includes("CERTIFICATE"))) {
    return `has a damaged BEGIN or END line on line ${n}`;
  }
  if (bodyLine.test(line) && line.replaceAll(line.charAt(0), "") !== "") {
    return `has certificate text without its BEGIN line on line ${n}`;
  }
  return undefined;
}

// blockTypes are the types of the bundle's blocks in turn, or what is wrong with its layout.
function blockTypes(bundle: string): string[] | string {
  const all = lines(bundle);
  const types: string[] = [];
  let open: string[] | undefined;
  let after = 0;
  for (const [i, { n, text }] of all.entries()) {
    if (open === undefined) {
      const problem = text.startsWith(begin) ? undefined : outsideProblem(text, n);
      if (problem !== undefined) {
        return problem;
      }
      open = text.startsWith(begin) ? [text] : undefined;
    } else if (text.startsWith(begin)) {
      return `block ${types.length + 1} is no complete PEM block`;
    } else if (text.startsWith(end)) {
      const type = blockType([...open, text]);
      if (type === undefined) {
        return `block ${types.length + 1} is no complete PEM block`;
      }
      types.push(type);
      open = undefined;
      after = i + 1;
    } else {
      open.push(text);
    }
  }
  if (open !== undefined) {
    return `block ${types.length + 1} is no complete PEM block`;
  }
  const trailing = all.slice(after).find((l) => trim(l.text) !== "");
  if (types.length > 0 && trailing !== undefined) {
    return `has text after its last PEM block on line ${trailing.n}`;
  }
  return types;
}

/** certificateBundleProblem says what is wrong with the layout of a bundle, in the server's words, or nothing. */
export function certificateBundleProblem(bundle: string, maxKiB = nodeKiB): string | undefined {
  // The server counts UTF-8 bytes.
  if (new TextEncoder().encode(bundle).length > maxKiB * kibibyte) {
    return `at most ${maxKiB} KiB`;
  }
  const types = blockTypes(bundle);
  if (typeof types === "string") {
    return types;
  }
  const other = types.find((t) => t !== "CERTIFICATE");
  if (other !== undefined) {
    return `holds a ${other.toLowerCase()}. Give certificates only`;
  }
  return types.length === 0 ? "holds no PEM certificate" : undefined;
}

/** sentence makes a problem the text of a field's error. */
export const sentence = (problem: string): string => `${problem.charAt(0).toUpperCase()}${problem.slice(1)}`;
