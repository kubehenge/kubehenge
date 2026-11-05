// Fails the build when a script the site ships cannot run in Safari before 17, which cannot parse
// a regular expression with the v flag and then loads none of the file. It reads the built output,
// so VitePress's and Vue's own scripts count too. Use the u flag instead.
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { parseSync } from "oxc-parser";

const dist = join(dirname(fileURLToPath(import.meta.url)), ".vitepress", "dist");

// Every script in the output: files, and the inline ones in the pages.
function* scripts(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true, recursive: true })) {
    if (!entry.isFile()) continue;
    const path = join(entry.parentPath, entry.name);
    if (/\.(?:m?js)$/u.test(entry.name)) {
      yield [path, readFileSync(path, "utf8")];
    } else if (entry.name.endsWith(".html")) {
      const html = readFileSync(path, "utf8");
      for (const m of html.matchAll(/<script(?![^>]*\ssrc=)[^>]*>([\s\S]*?)<\/script>/gu)) {
        if (!/type="(?:application\/(?:ld\+)?json|speculationrules)"/u.test(m[0]))
          yield [`${path} (inline script)`, m[1]];
      }
    }
  }
}

function* nodes(node) {
  if (Array.isArray(node)) {
    for (const item of node) yield* nodes(item);
  } else if (node !== null && typeof node === "object") {
    yield node;
    for (const value of Object.values(node)) yield* nodes(value);
  }
}

// A literal with the flag, or a RegExp call given a flags string with it.
export function unicodeSets(source) {
  const { program, errors } = parseSync("script.js", source);
  if (errors.length > 0) throw new Error(errors[0].message);
  const found = [];
  for (const node of nodes(program)) {
    if (node.type === "Literal" && node.regex?.flags.includes("v")) {
      found.push(`/${node.regex.pattern}/${node.regex.flags}`);
    } else if (
      (node.type === "NewExpression" || node.type === "CallExpression") &&
      node.callee.type === "Identifier" &&
      node.callee.name === "RegExp" &&
      node.arguments[1]?.type === "Literal" &&
      typeof node.arguments[1].value === "string" &&
      node.arguments[1].value.includes("v")
    ) {
      found.push(`RegExp(…, "${node.arguments[1].value}")`);
    }
  }
  return found;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  let files = 0;
  const failures = [];
  for (const [name, source] of scripts(dist)) {
    files++;
    try {
      for (const found of unicodeSets(source)) failures.push(`${name}: ${found}`);
    } catch (error) {
      failures.push(`${name}: ${error.message}`);
    }
  }
  if (files === 0) throw new Error(`No scripts found in ${dist}: build the site first.`);
  if (failures.length > 0) {
    console.error(
      `Regular expressions with the v flag, which Safari before 17 cannot parse (use u):\n${failures.join("\n")}`,
    );
    process.exit(1);
  }
  console.log(`check-scripts: ${files} scripts, none with a v-flag regular expression.`);
}
