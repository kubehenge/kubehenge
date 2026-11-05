// Fails when the home page composites a layer much larger than the window. WebKit on a phone kills
// the whole tab once the tiles of such a layer outgrow its memory, with no error to report, and
// Firefox repaints large moving layers every frame. It serves the built site on loopback, plays the
// page's animation in headless Chromium and reads the compositor's layer tree (CDP LayerTree) at
// a phone and a desktop size.
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { dirname, extname, join, posix } from "node:path";
import { fileURLToPath } from "node:url";

import { chromium } from "playwright";

const dist = join(dirname(fileURLToPath(import.meta.url)), ".vitepress", "dist");
const LIMIT = 1.5; // windows, in each direction
const PLAY_MS = Number(process.env.KUBEHENGE_LAYERS_MS ?? 16_000);

const types = {
  ".html": "text/html",
  ".js": "text/javascript",
  ".css": "text/css",
  ".woff2": "font/woff2",
  ".svg": "image/svg+xml",
};

const server = createServer(async (req, res) => {
  const path = posix.normalize(decodeURIComponent(new URL(req.url, "http://x").pathname));
  const file = join(dist, path.endsWith("/") ? `${path}index.html` : path);
  try {
    const body = await readFile(extname(file) ? file : `${file}.html`);
    res.writeHead(200, { "content-type": types[extname(file)] ?? "text/html" }).end(body);
  } catch {
    res.writeHead(404).end();
  }
});

// The sizes a layer reaches while the page plays and the reader scrolls it end to end.
async function layersOf(browser, context, viewport) {
  const page = await browser.newPage({ ...context, viewport });
  const cdp = await page.context().newCDPSession(page);
  const seen = new Map();
  cdp.on("LayerTree.layerTreeDidChange", ({ layers }) => {
    for (const layer of layers ?? []) {
      const id = `${layer.layerId}`;
      const old = seen.get(id);
      // A later report of a layer may leave out its node, so the first one named stays.
      const named = { ...layer, backendNodeId: layer.backendNodeId ?? old?.backendNodeId };
      if (!old || layer.width * layer.height > old.width * old.height) seen.set(id, named);
      else if (old.backendNodeId === undefined) old.backendNodeId = named.backendNodeId;
    }
  });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(`${origin}/`, { waitUntil: "load" });
  // The domain is enabled after the load, as a navigation drops the agent's view of the tree. It
  // reports nodes only to a client that has the DOM domain open.
  await cdp.send("DOM.enable");
  await cdp.send("LayerTree.enable");
  const height = await page.evaluate(() => document.documentElement.scrollHeight);
  const steps = 8;
  for (let i = 0; i <= steps; i++) {
    await page.evaluate((y) => window.scrollTo(0, y), (height * i) / steps);
    await page.waitForTimeout(PLAY_MS / (steps + 1));
  }
  await cdp.send("LayerTree.disable");
  // The layer of the scrolled document is as tall as the page. It does not move on its own and the
  // browser tiles only the part near the window, so it is not what this check is about.
  const layers = [];
  for (const layer of seen.values()) {
    const { node } = await cdp
      .send("DOM.describeNode", { backendNodeId: layer.backendNodeId ?? 0 })
      .catch(() => ({ node: {} }));
    if (node.nodeName !== "#document") layers.push({ ...layer, node: node.nodeName ?? "none" });
  }
  await page.close();
  if (errors.length > 0) throw new Error(`Page errors: ${errors.join("; ")}`);
  return layers;
}

let origin = "";
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({
  headless: true,
  channel: "chromium",
  args: ["--disable-features=OmniboxEverywhere", "--mute-audio"],
});
const failures = [];
try {
  const forms = [
    ["phone", { width: 390, height: 844 }, { deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
    ["desktop", { width: 1440, height: 900 }, {}],
  ];
  for (const [name, viewport, context] of forms) {
    const layers = await layersOf(browser, context, viewport);
    const largest = layers.reduce((m, l) => (l.width * l.height > m.width * m.height ? l : m), {
      width: 0,
      height: 0,
      node: "none",
    });
    console.log(
      `check-layers ${name} ${viewport.width}x${viewport.height}: ${layers.length} layers, largest ${largest.width}x${largest.height} (${largest.node.toLowerCase()})`,
    );
    for (const l of layers) {
      if (l.width > viewport.width * LIMIT || l.height > viewport.height * LIMIT) {
        failures.push(`${name}: layer ${l.layerId} is ${l.width}x${l.height} (${l.node.toLowerCase()})`);
      }
    }
  }
  // A reader who asks for less motion gets a page where nothing is animating once it has loaded.
  const calm = await browser.newPage({ viewport: { width: 1440, height: 900 }, reducedMotion: "reduce" });
  await calm.goto(`${origin}/`, { waitUntil: "load" });
  await calm.waitForTimeout(3000);
  const moving = await calm.evaluate(() =>
    document
      .getAnimations()
      .filter((a) => a.playState === "running")
      .map((a) => a.effect?.target?.className || a.effect?.target?.localName || "unknown"),
  );
  await calm.close();
  console.log(`check-layers reduced motion: ${moving.length} running animations`);
  if (moving.length > 0) failures.push(`reduced motion: still animating: ${[...new Set(moving)].join(", ")}`);
} finally {
  await browser.close();
  server.close();
}
if (failures.length > 0) {
  console.error(`Composited layers over ${LIMIT} times the window:\n${failures.join("\n")}`);
  process.exit(1);
}
