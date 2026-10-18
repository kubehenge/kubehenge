// Copies the documentation and the logo into the site, so docs/ stays plain Markdown that reads
// the same on GitHub, and nothing outside docs/ can end up on the site.
import { copyFileSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const site = dirname(fileURLToPath(import.meta.url));
const docs = join(site, "..", "docs");
const pages = join(site, "docs");

// Search results and link previews quote a page's description, so each page gets its opening
// paragraph's first sentences, up to about what a search result shows.
const descriptionLength = 200;

function description(markdown) {
  const lines = markdown.split("\n");
  const start = lines.findIndex((line, i) => i > 0 && line.trim() !== "" && !/^(?:#|[-*>|]|\d+\.|```|<)/u.test(line));
  if (start === -1) {
    return undefined;
  }
  const end = lines.findIndex((line, i) => i > start && line.trim() === "");
  const text = lines
    .slice(start, end === -1 ? undefined : end)
    .join(" ")
    .replaceAll(/\[([^\]]*)\]\([^)]*\)/gu, "$1")
    .replaceAll(/[`*_]/gu, "")
    .replaceAll(/\s+/gu, " ")
    .trim();
  // A sentence ends at a stop before a space, so 1.0.0 and file names stay whole.
  const sentences = text.match(/.+?[.!?](?=\s|$)/gu) ?? [text];
  let kept = sentences[0].trim();
  for (const s of sentences.slice(1)) {
    if (kept.length + s.length > descriptionLength) {
      break;
    }
    kept += s;
  }
  return kept.trim();
}

rmSync(pages, { recursive: true, force: true });
mkdirSync(pages);
for (const name of readdirSync(docs)) {
  if (name.endsWith(".md")) {
    const markdown = readFileSync(join(docs, name), "utf8");
    const said = description(markdown);
    // JSON's quoting is valid YAML, whatever the sentence holds.
    const front = said === undefined ? "" : `---\ndescription: ${JSON.stringify(said)}\n---\n\n`;
    writeFileSync(join(pages, name), front + markdown);
  }
}
mkdirSync(join(site, "public"), { recursive: true });
copyFileSync(join(site, "..", "web", "src", "logo.svg"), join(site, "public", "logo.svg"));
