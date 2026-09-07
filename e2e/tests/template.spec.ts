import { test, expect } from '@playwright/test';
import { createBoard } from './helpers';

test('new-board picker defaults to went well / better / kudos', async ({
  page,
}) => {
  await page.goto('/new');
  const chosen = page.locator('input[name="template"]:checked');
  await expect(chosen).toHaveValue('went-well-better-kudos');
  await expect(page.getByText('Went well / Better / Kudos')).toBeVisible();
});

test('creating a board without picking a template seeds the default columns', async ({
  page,
}) => {
  const stamp = Date.now().toString(36);
  await createBoard(page, { name: `Default template ${stamp}` });

  const columns = page.locator('section.column');
  await expect(columns).toHaveCount(3);
  await expect(columns.nth(0).locator('h3')).toContainText('Went well');
  await expect(columns.nth(1).locator('h3')).toContainText('Could be better');
  await expect(columns.nth(2).locator('h3')).toContainText('Kudos');

  await page.getByRole('button', { name: /facilitator controls/i }).click();
  await page.getByRole('button', { name: /^1 min$/i }).click();
  await expect(page.locator('#timer')).toContainText(/\d+:\d+ left/, {
    timeout: 8000,
  });
});
