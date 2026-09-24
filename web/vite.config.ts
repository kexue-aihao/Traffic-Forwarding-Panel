import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
const output = resolve(import.meta.dirname, "../internal/webui/assets");
export default defineConfig({
  base: "/assets/",
  publicDir: false,
  plugins: [
    vue(),
    {
      name: "local-assets",
      transformIndexHtml: {
        order: "post",
        handler: () => [
          {
            tag: "script",
            attrs: { src: "/assets/theme.js" },
            injectTo: "head-prepend",
          },
        ],
      },
      closeBundle() {
        mkdirSync(output, { recursive: true });
        copyFileSync("static/theme.js", resolve(output, "theme.js"));
        copyFileSync("static/favicon.svg", resolve(output, "favicon.svg"));
        copyFileSync(
          "node_modules/@fontsource-variable/inter/files/inter-latin-wght-normal.woff2",
          resolve(output, "inter.woff2"),
        );
        copyFileSync(
          "node_modules/@fontsource-variable/inter/LICENSE",
          resolve(output, "inter-LICENSE.txt"),
        );
        copyFileSync(
          "node_modules/lucide-static/LICENSE",
          resolve(output, "lucide-LICENSE.txt"),
        );
        const names = [
          "activity",
          "radar",
          "arrow-right-left",
          "server",
          "users",
          "wallet",
          "layers",
          "shield",
          "log-out",
          "menu",
          "plus",
          "sun",
          "moon",
          "settings",
          "book-open",
        ];
        const symbols = names.map((name) => {
          const raw = readFileSync(
            `node_modules/lucide-static/icons/${name}.svg`,
            "utf8",
          );
          return `<symbol id="${name}" viewBox="0 0 24 24">${raw.replace(/^[\s\S]*?<svg[^>]*>/, "").replace(/<\/svg>[\s\S]*$/, "")}</symbol>`;
        });
        writeFileSync(
          resolve(output, "icons.svg"),
          `<svg xmlns="http://www.w3.org/2000/svg">${symbols.join("")}</svg>`,
        );
      },
    },
  ],
  define: { __VUE_OPTIONS_API__: false, __VUE_PROD_DEVTOOLS__: false },
  build: {
    outDir: output,
    emptyOutDir: true,
    cssCodeSplit: false,
    assetsInlineLimit: 0,
    sourcemap: false,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        entryFileNames: "app.js",
        chunkFileNames: "chunks/[name]-[hash].js",
        assetFileNames: (asset) =>
          asset.name?.endsWith(".css") ? "app.css" : "[name][extname]",
      },
    },
  },
});
