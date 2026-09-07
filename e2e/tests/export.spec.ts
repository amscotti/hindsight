import { test, expect } from '@playwright/test';
import { pickTemplate, newContext } from './helpers';
import * as fs from 'node:fs';

// The export loop: seeded content on the board must arrive in the
// downloaded Markdown, named as an .md attachment.
test('export downloads markdown containing the seeded card text', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const boardName = `Export e2e ${stamp}`;
  const cardBody = `export card ${stamp}`;

  const ctx = await newContext(browser);
  const page = await ctx.newPage();

  try {
    await page.goto('/new');
    await page.locator('input[name="name"]').fill(boardName);
    await page.locator('input[name="display_name"]').fill('Facilitator');
    await pickTemplate(page, 'mad-sad-glad');
    await page.getByRole('button', { name: /create board/i }).click();
    await expect(page).toHaveURL(/\/b\//);

    const col = await page.locator('section.column').nth(0).getAttribute('id');
    await page.locator(`#${col} input[name="body"]`).fill(cardBody);
    await page.locator(`#${col} > form button[type="submit"]`).click();
    await expect(
      page.locator(`#${col} article.card`).getByText(cardBody),
    ).toBeVisible({ timeout: 8000 });

    const downloadPromise = page.waitForEvent('download');
    await page.locator('#export-download').click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/\.md$/);
    // Chromium hands the navigation to its download manager, so no page
    // 'response' event ever fires for it. Assert the headers through the
    // context's API request, which shares the facilitator cookies.
    const response = await page.request.get(download.url());
    expect(response.status()).toBe(200);
    expect(response.headers()['content-disposition']).toMatch(
      /attachment;.*\.md/,
    );
    expect(response.headers()['content-type']).toContain('text/markdown');
    const path = await download.path();
    expect(path).toBeTruthy();
    const content = fs.readFileSync(path as string, 'utf-8');
    expect(content).toContain(cardBody);
    expect(content).toContain(boardName);
  } finally {
    await ctx.close();
  }
});
