import { test, expect } from '@playwright/test';

test('landing renders with local assets only', async ({ page }) => {
  const external: string[] = [];
  page.on('request', (req) => {
    const url = req.url();
    if (url.startsWith('data:') || url.startsWith('about:')) {
      return;
    }
    if (!/^https?:\/\/(localhost|127\.0\.0\.1)(:\d+)?\//.test(url)) {
      external.push(url);
    }
  });

  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Hindsight' })).toBeVisible();
  await expect(page.getByRole('button', { name: /start a new retro/i })).toBeVisible();
  await expect(page.locator('main')).toBeVisible();

  expect(external).toEqual([]);
});

test('/healthz returns ok', async ({ request }) => {
  const res = await request.get('/healthz');
  expect(res.status()).toBe(200);
  expect((await res.text()).trim()).toBe('ok');
});
