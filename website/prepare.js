// Copies the documentation and the logo into the site, so docs/ stays plain Markdown that reads
// the same on GitHub, and nothing outside docs/ can end up on the site.
import { copyFileSync, mkdirSync, readdirSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const site = dirname(fileURLToPath(import.meta.url));
const docs = join(site, "..", "docs");
const pages = join(site, "docs");

rmSync(pages, { recursive: true, force: true });
mkdirSync(pages);
for (const name of readdirSync(docs)) {
  if (name.endsWith(".md")) {
    copyFileSync(join(docs, name), join(pages, name));
  }
}
mkdirSync(join(site, "public"), { recursive: true });
copyFileSync(join(site, "..", "web", "src", "logo.svg"), join(site, "public", "logo.svg"));
