import { test, expect } from '@playwright/test';
import { createBoard, boardIdFromUrl } from './helpers';

test('dashboard search, archive, and duplicate keep the board findable', async ({
  page,
}) => {
  const stamp = Date.now().toString(36);
  const name = `Dash e2e ${stamp}`;
  const bid = await createBoard(page, { name, template: 'mad-sad-glad' });

  await page.goto('/');
  await expect(page.locator(`#board-${bid}`)).toBeVisible();
  await expect(page.locator(`#board-${bid}`).getByRole('link', { name })).toBeVisible();

  await page.locator('#board-search').fill(name);
  await page.getByRole('button', { name: /^search$/i }).click();
  await expect(page.locator(`#board-${bid}`)).toBeVisible();

  await page.locator('#board-search').fill(`no-such-board-${stamp}`);
  await page.getByRole('button', { name: /^search$/i }).click();
  await expect(page.locator(`#board-${bid}`)).toHaveCount(0);

  await page.goto('/');
  await page.locator(`#board-${bid}`).getByRole('button', { name: /^archive$/i }).click();
  await expect(page.getByRole('link', { name: 'Archived' })).toBeVisible();
  await page.getByRole('link', { name: 'Archived' }).click();
  await expect(page.locator(`#board-${bid}`)).toBeVisible();

  await page.locator(`#board-${bid}`).getByRole('button', { name: /^unarchive$/i }).click();
  await page.getByRole('link', { name: 'Active' }).click();
  await expect(page.locator(`#board-${bid}`)).toBeVisible();

  await page.locator(`#board-${bid}`).locator('summary').click();
  await page.locator(`#board-${bid}`).getByRole('button', { name: /^duplicate$/i }).click();
  await expect(page).toHaveURL(/\/b\//);
  const copyId = boardIdFromUrl(page.url());
  expect(copyId).not.toBe(bid);
  await expect(page.locator('section.column')).toHaveCount(3);
});
