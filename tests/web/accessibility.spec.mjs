import { createRequire } from "node:module";

const require = createRequire(new URL("../../apps/spillway/web/package.json", import.meta.url));
const AxeBuilder = require("@axe-core/playwright");
const { expect, test } = require("@playwright/test");

async function connectWithoutUsageStore(page) {
  await page.route("**/api/budget", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
  await page.route("**/api/providers", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ providers: [] }) }));
  await page.route("**/api/pricing", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prices: {} }) }));
  await page.route("**/api/usage**", (route) => route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { code: "store_disabled", message: "Usage storage is not configured." } }) }));
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("heading", { name: "Request playground" })).toBeVisible();
}

async function expectNoAccessibilityViolations(page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(results.violations).toEqual([]);
}

test("dashboard onboarding meets WCAG checks at a narrow dark viewport", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 900 });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await page.goto("/dashboard");

  await expectNoAccessibilityViolations(page);
  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: /Spillway/ })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("textbox", { name: "Dashboard key" })).toBeFocused();
  const dimensions = await page.evaluate(() => ({ viewport: document.documentElement.clientWidth, content: document.documentElement.scrollWidth }));
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);

  const motion = await page.evaluate(() => {
    const probe = document.createElement("div");
    probe.style.cssText = "animation-duration: 1s; transition-duration: 1s";
    document.body.append(probe);
    const computed = getComputedStyle(probe);
    const result = { animation: parseFloat(computed.animationDuration), transition: parseFloat(computed.transitionDuration), reduced: matchMedia("(prefers-reduced-motion: reduce)").matches };
    probe.remove();
    return result;
  });
  expect(motion.reduced).toBe(true);
  expect(motion.animation).toBeLessThanOrEqual(0.01);
  expect(motion.transition).toBeLessThanOrEqual(0.01);
});

test("connected dashboard meets WCAG checks in light and dark desktop and mobile views", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.emulateMedia({ colorScheme: "light" });
  await connectWithoutUsageStore(page);
  await expectNoAccessibilityViolations(page);

  await page.emulateMedia({ colorScheme: "dark" });
  await expectNoAccessibilityViolations(page);

  await page.setViewportSize({ width: 320, height: 900 });
  await expectNoAccessibilityViolations(page);
  const dimensions = await page.evaluate(() => ({ viewport: document.documentElement.clientWidth, content: document.documentElement.scrollWidth }));
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);
});
