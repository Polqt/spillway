import { expect, test } from "@playwright/test";

test("dashboard renders safe aggregate evaluation summaries", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    expect(request.headers().authorization).toBe("Bearer sk-test-dashboard");
    const path = new URL(request.url()).pathname;
    const body = path === "/api/budget" ? {}
      : path === "/api/providers" ? { providers: [] }
        : path === "/api/pricing" ? { prices: {} }
          : path === "/api/quotas" ? { candidates: [] }
            : path === "/api/evaluations" ? { data: [{
              run_id: "eval-safe-001",
              created_at: "2026-10-19T09:00:00Z",
              corpus_sha256: "a".repeat(64),
              summary: { prompt_count: 42, results: [{ arm: "rules", gate: "request_success_difference", verdict: "PASS", difficulty: "all", category: "all" }] },
            }] }
              : { data: [] };
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  });

  await page.goto("/");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();

  await expect(page.getByRole("heading", { name: "Evaluation summaries" })).toBeVisible();
  await expect(page.getByText("eval-safe-001")).toBeVisible();
  await expect(page.getByText(/42 prompts · corpus a{12}/)).toBeVisible();
  await expect(page.getByText("rules request_success_difference: PASS")).toBeVisible();
  await expect(page.locator("body")).not.toContainText("private prompt");
  await expect(page.locator("body")).not.toContainText("private response");
});
