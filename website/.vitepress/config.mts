import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import Beasties from "beasties";
import { defineConfig, type HeadConfig } from "vitepress";

const repository = "https://github.com/kubehenge/kubehenge";
const site = "https://kubehenge.dev";

// The home page's fonts that sit above the fold, fetched at once instead of when the stylesheet is parsed.
const homeFonts =
  /\/(JetBrainsMono-(Regular|Bold)|AtkinsonHyperlegibleNext-400|BigShouldersDisplay-(800|900))\.[^/]+\.woff2$/u;

const sha256 = (text: string) => `'sha256-${createHash("sha256").update(text).digest("base64")}'`;

// A page may run only its own scripts and the inline ones VitePress writes into it, which are
// listed by hash. GitHub Pages sets no headers, so this travels in the page. The inline style
// attributes Vue renders are listed by hash too, and styles set from script are not restricted.
function contentSecurityPolicy(html: string) {
  const scripts = [...html.matchAll(/<script(?![^>]*\ssrc=)[^>]*>([\s\S]*?)<\/script>/gu)].map((m) => sha256(m[1]));
  const styles = [...html.matchAll(/\sstyle="([^"]*)"/gu)].map((m) => sha256(m[1]));
  const handlers = [...html.matchAll(/\sonload="([^"]*)"/gu)].map((m) => sha256(m[1]));
  const sheets = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gu)].map((m) => sha256(m[1]));
  return [
    "default-src 'self'",
    `script-src 'self' ${[...new Set(scripts)].join(" ")}`,
    `script-src-attr 'unsafe-hashes' ${[...new Set(handlers)].join(" ")}`,
    `style-src 'self' ${[...new Set(sheets)].join(" ")}`,
    `style-src-attr 'unsafe-hashes' ${[...new Set(styles)].join(" ")}`,
    "img-src 'self' data:",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "require-trusted-types-for 'script'",
  ].join("; ");
}

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");

// Each page's fingerprint and the day it last changed. Search engines trust a sitemap date only
// if it moves when the page does, and IndexNow is sent only the pages whose fingerprint moved. The
// state lives on the published site (/pages.json), never in git, as a committed copy made every
// pull request that touches a page conflict with the others. A deployment build starts from the
// published file, which the workflow fetches and PAGES_PREVIOUS names. Without it, as on a local
// or pull request build, every page starts at its source's last commit.
const previousFile = process.env.PAGES_PREVIOUS ?? "";
const pagesName = "pages.json";

interface Page {
  fingerprint: string;
  lastmod: string;
}

// fingerprint hashes what a reader or a search result shows of a page: its title, description,
// main text and images. The doc footer (edit link, dates, next and previous pages) and the content
// hashes in asset names are left out, as they move without the page.
function fingerprint(html: string): string {
  const head = [/<title>([\s\S]*?)<\/title>/u, /<meta name="description" content="([^"]*)"/u].map(
    (pattern) => pattern.exec(html)?.[1] ?? "",
  );
  const main = (/<main[\s>][\s\S]*<\/main>/u.exec(html)?.[0] ?? "")
    .replace(/<footer class="VPDocFooter"[\s\S]*?<\/footer>/u, "")
    .replaceAll(/<(script|style)\b[\s\S]*?<\/\1>/gu, "")
    .replaceAll(/<!--[\s\S]*?-->/gu, "")
    .replaceAll(/\sdata-v-\w+(?:="[^"]*")?/gu, "");
  const images = [...main.matchAll(/<img\b[^>]*>|<svg\b[\s\S]*?<\/svg>/gu)].map((m) =>
    m[0].replaceAll(/(\/assets\/[^"]+)\.[\w-]{8}(\.\w+")/gu, "$1$2"),
  );
  const text = main
    .replaceAll(/<[^>]+>/gu, " ")
    .replaceAll(/\s+/gu, " ")
    .trim();
  return createHash("sha256")
    .update(JSON.stringify([head, images, text]))
    .digest("hex");
}

// A page with no earlier date starts at its source's last commit, or today while that is uncommitted.
function firstSeen(url: string, today: string): string {
  const source = url === "" ? "website/index.md" : `${url}.md`;
  const day = execFileSync("git", ["log", "-1", "--format=%cs", "--", source], { cwd: root, encoding: "utf8" });
  return day.trim() || today;
}

export default defineConfig({
  title: "Kubehenge",
  description: "Kubernetes for Proxmox VE",
  lang: "en-GB",
  cleanUrls: true,
  lastUpdated: true,
  // GitHub's heading anchors, which the docs link to so that a link works on GitHub and here alike,
  // and which scripts/changelog.sh checks the changelog's links against. VitePress's own turn an
  // apostrophe into a hyphen, where GitHub drops it.
  markdown: {
    anchor: {
      slugify: (heading) =>
        heading
          .toLowerCase()
          .replaceAll(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, "")
          .replaceAll(" ", "-"),
    },
  },
  // VueUse's dark-mode switch writes a style element and reads it back to stop transitions, which
  // makes the browser lay the page out again at start-up. The theme animates the switch itself.
  appearance: { disableTransition: false },
  head: [
    ["link", { rel: "icon", type: "image/svg+xml", href: "/logo.svg" }],
    // What chat and social apps show for a shared link: Open Graph, and X's own card tags.
    ["meta", { property: "og:site_name", content: "Kubehenge" }],
    ["meta", { property: "og:type", content: "website" }],
    ["meta", { property: "og:locale", content: "en_GB" }],
    ["meta", { property: "og:image", content: `${site}/og.png` }],
    ["meta", { property: "og:image:width", content: "1200" }],
    ["meta", { property: "og:image:height", content: "630" }],
    ["meta", { property: "og:image:alt", content: "Kubehenge: Kubernetes for Proxmox VE, a ring of stones" }],
    ["meta", { name: "twitter:card", content: "summary_large_image" }],
  ],
  // Each page's own title, description and address, and the address search engines index it at.
  transformHead({ pageData, title, description, assets }) {
    const path = pageData.relativePath.replace(/(?:^|\/)index\.md$/u, "").replace(/\.md$/u, "");
    const url = `${site}/${path}`;
    const fonts: HeadConfig[] =
      pageData.relativePath === "index.md"
        ? assets
            .filter((file) => homeFonts.test(file))
            .map((file) => ["link", { rel: "preload", href: file, as: "font", type: "font/woff2", crossorigin: "" }])
        : [];
    return [
      ...fonts,
      ["link", { rel: "canonical", href: url }],
      ["meta", { property: "og:url", content: url }],
      ["meta", { property: "og:title", content: title }],
      ["meta", { property: "og:description", content: description }],
    ];
  },
  // The theme preloads Inter and the icon stylesheet for every page. The home page uses neither, so
  // it drops them. The content security policy goes last, as it lists the page's inline scripts.
  async transformHtml(code, _id, { pageData, siteConfig }) {
    let html = code;
    if (pageData.relativePath === "index.md") {
      html = html
        .replace(/\s*<link rel="preload" href="[^"]*inter-[^"]*\.woff2"[^>]*>/u, "")
        .replace(/\s*<link rel="preload stylesheet" href="[^"]*vp-icons[^"]*"[^>]*>/u, "");
    }
    // The stylesheet blocks the first paint, so each page carries the rules it paints with and the
    // whole sheet follows at the end of the body, where it blocks nothing.
    const beasties = new Beasties({
      path: siteConfig.outDir,
      publicPath: "/",
      preload: "media",
      keyframes: "all",
      pruneSource: false,
      logLevel: "warn",
    });
    html = await beasties.process(
      html.replace(/<link rel="preload stylesheet"( href="[^"]*\/style\.[^"]*")[^>]*?>/u, '<link rel="stylesheet"$1>'),
    );
    // Beasties finds a font family only where a rule names it, and these pages name theirs through
    // custom properties, so it would leave every @font-face to the late sheet. They go in whole.
    const sheet = /<link rel="stylesheet" href="([^"]+\.css)">/u.exec(html)?.[1];
    if (sheet !== undefined) {
      const faces = readFileSync(join(siteConfig.outDir, sheet), "utf8").match(/@font-face\{[^}]*\}/gu) ?? [];
      html = html.replace("</head>", `<style>${faces.join("")}</style>\n  </head>`);
    }
    const policy = contentSecurityPolicy(html).replaceAll("&", "&amp;").replaceAll('"', "&quot;");
    return html.replace("<head>", `<head>\n    <meta http-equiv="Content-Security-Policy" content="${policy}">`);
  },
  // The icon stylesheet's file name is final only once every page is written, so pages that use it
  // get it inline now, with its hash in their policy.
  buildEnd({ outDir }) {
    const icons = /<link rel="preload stylesheet" href="(\/assets\/vp-icons\.[^"]+\.css)" as="style">/u;
    for (const file of readdirSync(outDir, { recursive: true, encoding: "utf8" })) {
      if (!file.endsWith(".html")) continue;
      const path = join(outDir, file);
      const html = readFileSync(path, "utf8");
      const href = icons.exec(html)?.[1];
      if (href === undefined) continue;
      const css = readFileSync(join(outDir, href), "utf8");
      writeFileSync(
        path,
        html
          .replace(icons, () => `<style>${css}</style>`)
          .replace("style-src 'self'", `style-src 'self' ${sha256(css)}`),
      );
    }
  },
  sitemap: {
    hostname: site,
    lastmodDateOnly: true,
    // Runs once every page is written, so each date comes from the page as built.
    transformItems(items) {
      // A named file that cannot be read fails the build, so a fault never resets every date.
      const known: Record<string, Page | undefined> =
        previousFile === "" ? {} : JSON.parse(readFileSync(previousFile, "utf8"));
      const today = new Date().toISOString().slice(0, 10);
      const pages: Record<string, Page> = {};
      for (const item of items) {
        const path = `/${item.url}`;
        const html = readFileSync(join(here, "dist", `${item.url === "" ? "index" : item.url}.html`), "utf8");
        const print = fingerprint(html);
        const was = known[path];
        const lastmod = was === undefined ? firstSeen(item.url, today) : today;
        const page = was?.fingerprint === print ? was : { fingerprint: print, lastmod };
        pages[path] = page;
        item.lastmod = page.lastmod;
      }
      const sorted = Object.fromEntries(Object.entries(pages).sort(([a], [b]) => a.localeCompare(b)));
      writeFileSync(join(here, "dist", pagesName), `${JSON.stringify(sorted, null, 2)}\n`);
      return items;
    },
  },
  themeConfig: {
    logo: { src: "/logo.svg", alt: "" },
    nav: [
      { text: "Docs", link: "/docs/install" },
      { text: "Changelog", link: "/docs/changelog" },
    ],
    sidebar: [
      {
        text: "Using Kubehenge",
        items: [
          { text: "Installing", link: "/docs/install" },
          { text: "Setup settings in depth", link: "/docs/install-settings" },
          { text: "Clusters", link: "/docs/clusters" },
          { text: "Upgrading", link: "/docs/upgrading" },
          { text: "Uninstalling", link: "/docs/uninstalling" },
          { text: "Troubleshooting", link: "/docs/troubleshooting" },
          { text: "VM templates", link: "/docs/templates" },
        ],
      },
      {
        text: "How it is made",
        items: [
          { text: "Architecture", link: "/docs/architecture" },
          { text: "Testing", link: "/docs/testing" },
          { text: "Releasing", link: "/docs/releasing" },
          { text: "Roadmap", link: "/docs/roadmap" },
          { text: "Changelog", link: "/docs/changelog" },
        ],
      },
    ],
    socialLinks: [{ icon: "github", link: repository }],
    editLink: { pattern: `${repository}/edit/main/:path`, text: "Change this page on GitHub" },
    search: { provider: "local" },
    footer: {
      message:
        "Apache-2.0. Not affiliated with or endorsed by Proxmox Server Solutions GmbH; Proxmox is its registered trademark.",
    },
  },
});
