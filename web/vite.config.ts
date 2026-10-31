import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { brotliCompressSync, constants, gzipSync } from "node:zlib";

import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";

// precompress writes a Brotli and a gzip form beside each text file of the build, once, so the
// interface serves the smallest one the browser takes without compressing anything per request.
// It compresses the files as written: Vite still rewrites chunks during generateBundle (its module
// preload placeholder among them), so a form made from the bundle there can differ from the file.
function precompress(): Plugin {
  return {
    name: "kubehenge-precompress",
    apply: "build",
    async writeBundle({ dir }, bundle) {
      if (dir === undefined) {
        throw new Error("precompress needs the build's output directory");
      }
      const text = Object.keys(bundle).filter((name) => /\.(?:html|js|css|svg|json)$/u.test(name));
      await Promise.all(
        text.map(async (name) => {
          const raw = await readFile(join(dir, name));
          const forms = {
            br: brotliCompressSync(raw, {
              params: {
                [constants.BROTLI_PARAM_QUALITY]: constants.BROTLI_MAX_QUALITY,
                [constants.BROTLI_PARAM_SIZE_HINT]: raw.length,
              },
            }),
            gz: gzipSync(raw, { level: 9 }),
          };
          // A form no smaller than the file is no use to anyone.
          const smaller = Object.entries(forms).filter(([, data]) => data.length < raw.length);
          await Promise.all(smaller.map(([suffix, data]) => writeFile(join(dir, `${name}.${suffix}`), data)));
        }),
      );
    },
  };
}

export default defineConfig({
  plugins: [react(), precompress()],
  build: {
    // Embedded into the kubehenge binary by pkg/ui.
    outDir: "../pkg/ui/dist",
    emptyOutDir: true,
    // The interface's Content-Security-Policy allows no inline scripts or styles.
    assetsInlineLimit: 0,
    // precompress writes the compressed files themselves.
    reportCompressedSize: false,
    rolldownOptions: {
      // A module loaded lazily and also imported outright stays in its importer's chunk, so the page
      // split off to load on its own loads with every view instead.
      onLog(level, log, handle) {
        if (log.code === "INEFFECTIVE_DYNAMIC_IMPORT") {
          throw new Error(log.message);
        }
        handle(level, log);
      },
    },
  },
  server: {
    // `kubehenge serve` on its default port, for `npm run dev`.
    proxy: { "/api": { target: "https://localhost:8443", secure: false } },
  },
  test: {
    // Half the time of jsdom for these tests, most of it in setting up each file's document.
    environment: "happy-dom",
    setupFiles: ["src/test-setup.ts"],
    // The wizard tests type and click through whole pages; run all at once on a loaded machine, they
    // can pass the default 5 s.
    testTimeout: 20_000,
  },
});
