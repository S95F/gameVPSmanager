import { test, expect } from '@playwright/test';

// A rendered-page smoke test for the dashboard at a desktop and a phone
// viewport (the two projects in playwright.config.ts). The mobile/tablet layout
// that landed had no rendered-page test; this covers the three pages an invited
// moderator actually uses — login, the instance list, and the live console —
// plus a no-horizontal-scroll check at phone width.
//
// It runs against tools/devpreview, which auto-authenticates, so it exercises
// page RENDERING and the SSE console pane, not the real login POST / TOTP (those
// stay covered by the backend-backed tests in internal/web).

test('the login page renders its password form', async ({ page }) => {
  const resp = await page.goto('/login');
  expect(resp?.status()).toBe(200);
  await expect(page.locator('input[type="password"]')).toBeVisible();
});

test('the instance list renders the fixtures', async ({ page }) => {
  const resp = await page.goto('/instances');
  expect(resp?.status()).toBe(200);
  await expect(page.getByText('pz-main')).toBeVisible();
  await expect(page.getByText('valheim-longname-eu-west-3b')).toBeVisible();
});

test('the console page streams a live line', async ({ page }) => {
  const resp = await page.goto('/i/pz-main/console');
  expect(resp?.status()).toBe(200);
  // devpreview emits a backlog immediately and then one line per second; the
  // pane's JS connects the SSE stream and appends them.
  await expect(page.locator('#console')).toContainText('[dev]', { timeout: 15_000 });
});

test('no horizontal page scroll on a phone', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'phone-width check only');
  await page.goto('/instances');
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  // A phone that has to scroll sideways to read the instance list is the bug
  // the mobile layout fixed; allow 1px for sub-pixel rounding.
  expect(overflow).toBeLessThanOrEqual(1);
});
