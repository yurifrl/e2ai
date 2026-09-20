import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  retries: 1,
  workers: 1,
  reporter: "json",
  use: { headless: true },
});
