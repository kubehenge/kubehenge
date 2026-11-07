// Lighthouse CI over the built site, served on loopback, for the form factor in
// KUBEHENGE_LIGHTHOUSE (mobile or desktop). Every limit is checked against the median of 5 runs,
// as single runs move by a few points on a busy machine.
const { chromium } = require("playwright");
const { default: lighthouseConfig } = require("lighthouse/core/config/default-config.js");

const formFactor = process.env.KUBEHENGE_LIGHTHOUSE ?? "mobile";
if (formFactor !== "mobile" && formFactor !== "desktop") {
  throw new Error(`KUBEHENGE_LIGHTHOUSE must be mobile or desktop, not ${formFactor}`);
}

// Every scored audit of these categories passes or fails outright, so each has to pass.
const binary = ["accessibility", "best-practices", "seo"].flatMap((category) =>
  lighthouseConfig.categories[category].auditRefs
    .filter((ref) => ref.weight > 0)
    .map((ref) => [ref.id, ["error", { minScore: 1 }]]),
);

// What each page loads for itself, as [requests, bytes] per resource type. A docs page goes by its
// file, as this server has no clean URLs. Bytes and requests do not vary between runs: bytes get 3
// to 5 per cent of room for a docs edit, requests none. The pages the sidebar's links prefetch
// arrive as type "other" and have no limit, since every docs page added grows them.
const own = {
  "/": {
    document: [1, 13_300],
    script: [6, 80_000],
    stylesheet: [1, 25_400],
    font: [6, 95_000],
    image: [1, 740],
  },
  "/docs/install.html": {
    document: [1, 40_000],
    script: [6, 80_000],
    stylesheet: [1, 25_400],
    font: [1, 76_000],
    image: [2, 19_700],
  },
};

// The performance score is a floor below the lowest of 20 runs.
const performance = {
  mobile: { "/": 0.93, "/docs/install.html": 0.84 },
  desktop: { "/": 0.99, "/docs/install.html": 0.99 },
}[formFactor];

// Layout shift stays near 0 only while no late file reflows the page, as a web font that arrives
// after the first paint does unless a stand-in with its metrics holds the place. A shift here is a
// defect, even when most runs miss it.
const cls = 0.01;

const escape = (path) => path.replaceAll(/[.*+?^${}()|[\]\\]/gu, "\\$&");

module.exports = {
  ci: {
    collect: {
      staticDistDir: ".vitepress/dist",
      url: Object.keys(own).map((path) => `http://localhost${path}`),
      numberOfRuns: Number(process.env.KUBEHENGE_LIGHTHOUSE_RUNS ?? 5),
      chromePath: chromium.executablePath(),
      settings: {
        ...(formFactor === "desktop" ? { preset: "desktop" } : {}),
        // Playwright's Chromium in its new headless mode, which lhci adds itself, with a fresh
        // temporary profile each run. Its search launcher would otherwise install itself on
        // Windows. One string, as lhci appends its own flag to it: an array would reach Chrome
        // joined by commas, as one malformed flag.
        chromeFlags: "--disable-features=OmniboxEverywhere",
      },
    },
    assert: {
      assertMatrix: Object.entries(own).map(([path, types]) => ({
        matchingUrlPattern: `^http://[^/]+${escape(path)}$`,
        aggregationMethod: "median-run",
        assertions: {
          ...Object.fromEntries(binary),
          "categories:performance": ["error", { minScore: performance[path] }],
          "cumulative-layout-shift": ["error", { maxNumericValue: cls }],
          ...Object.fromEntries(
            Object.entries(types).flatMap(([type, [requests, bytes]]) => [
              [`resource-summary:${type}:count`, ["error", { maxNumericValue: requests }]],
              [`resource-summary:${type}:size`, ["error", { maxNumericValue: bytes }]],
            ]),
          ),
        },
      })),
    },
    upload: {
      target: "filesystem",
      outputDir: `.vitepress/lighthouse/${formFactor}`,
    },
  },
};
