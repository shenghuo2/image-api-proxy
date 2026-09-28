import { defineConfig } from "vitest/config";
import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        bindings: {
          PROXY_ADMIN_KEY: "test-admin-key-not-a-real-secret-1234567890",
        },
      },
    }),
  ],
  test: {
    reporters: ["default", "./test/metrics-reporter.mjs"],
    testTimeout: 30_000,
    hookTimeout: 30_000,
    fileParallelism: false,
  },
});
