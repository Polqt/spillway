import { defineConfig } from "@playwright/test";

export default defineConfig({
  fullyParallel: true,
  reporter: "list",
  projects: [
    { name: "dashboard", testDir: "./e2e" },
    { name: "accessibility", testDir: "../../../tests/web" },
  ],
  use: {
    baseURL: "http://127.0.0.1:4173",
    browserName: "chromium",
    headless: true,
  },
  webServer: {
    command: "pnpm run preview",
    port: 4173,
    reuseExistingServer: !process.env.CI,
  },
});
