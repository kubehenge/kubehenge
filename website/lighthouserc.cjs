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

// Limits per page and form factor. A docs page goes by its file, as this server has no clean URLs.
// The performance score is a floor below the lowest of 20 runs. Layout shift, bytes and requests
// do not vary between runs: bytes get 3 to 5 per cent of room for a docs edit, requests none.
const limits = {
  mobile: {
    "/": { performance: 0.93, cls: 0.01, bytes: 215_000, fontBytes: 95_000, requests: 16 },
    "/docs/install.html": {
      performance: 0.84,
      cls: 0.01,
      bytes: 285_000,
      fontBytes: 76_000,
      requests: 14,
    },
  },
  desktop: {
    "/": { performance: 0.99, cls: 0.01, bytes: 215_000, fontBytes: 95_000, requests: 16 },
    "/docs/install.html": {
      performance: 0.99,
      cls: 0.01,
      bytes: 470_000,
      fontBytes: 76_000,
      requests: 25,
    },
  },
}[formFactor];

const escape = (path) => path.replaceAll(/[.*+?^${}()|[\]\\]/gu, "\\$&");

module.exports = {
  ci: {
    collect: {
      staticDistDir: ".vitepress/dist",
      url: Object.keys(limits).map((path) => `http://localhost${path}`),
      numberOfRuns: Number(process.env.KUBEHENGE_LIGHTHOUSE_RUNS ?? 5),
      chromePath: chromium.executablePath(),
      settings: {
        ...(formFactor === "desktop" ? { preset: "desktop" } : {}),
        // Playwright's Chromium in its new headless mode, a fresh temporary profile each run. Its
        // search launcher would otherwise install itself on Windows. Linux runners' AppArmor
        // refuses the user namespaces its sandbox needs.
        chromeFlags: [
          "--headless=new",
          "--disable-features=OmniboxEverywhere",
          ...(process.platform === "linux" ? ["--no-sandbox"] : []),
        ],
      },
    },
    assert: {
      assertMatrix: Object.entries(limits).map(([path, limit]) => ({
        matchingUrlPattern: `^http://[^/]+${escape(path)}$`,
        aggregationMethod: "median-run",
        assertions: {
          ...Object.fromEntries(binary),
          "categories:performance": ["error", { minScore: limit.performance }],
          "cumulative-layout-shift": ["error", { maxNumericValue: limit.cls }],
          "total-byte-weight": ["error", { maxNumericValue: limit.bytes }],
          "resource-summary:font:size": ["error", { maxNumericValue: limit.fontBytes }],
          "resource-summary:total:count": ["error", { maxNumericValue: limit.requests }],
        },
      })),
    },
    upload: {
      target: "filesystem",
      outputDir: `.vitepress/lighthouse/${formFactor}`,
    },
  },
};
