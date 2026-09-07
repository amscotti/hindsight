import { test, expect } from '@playwright/test';
import { createBoard, joinBoard, twoViewers, waitForLive } from './helpers';

test('kudos and action items posted in A appear live in B', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const thanks = `thanks ${stamp}`;
  const next = `ship the fix ${stamp}`;
  const { pageA, pageB, close } = await twoViewers(browser);

  try {
    const bid = await createBoard(pageA, { name: `Walls e2e ${stamp}` });
    await joinBoard(pageB, bid, 'Watcher');
    await waitForLive(pageA);
    await waitForLive(pageB);

    await pageA.locator('#kudos-wall input[name="to"]').fill('Bo');
    await pageA.locator('#kudos-wall input[name="body"]').fill(thanks);
    await pageA.locator('#kudos-wall button[type="submit"]').click();
    await expect(pageB.locator('#kudos-list').getByText(thanks)).toBeVisible({
      timeout: 1000,
    });
    await expect(pageA.locator('#kudos-list').getByText(thanks)).toBeVisible();

    await pageA.locator('#actions-wall input[name="text"]').fill(next);
    await pageA.locator('#actions-wall input[name="owner"]').fill('Ada');
    await pageA.locator('#actions-wall button[type="submit"]').click();
    const action = pageB.locator('#actions-list article.action').filter({
      hasText: next,
    });
    await expect(action).toBeVisible({ timeout: 1000 });
    await expect(action).toContainText('Ada');

    await pageA
      .locator('#actions-list article.action')
      .filter({ hasText: next })
      .getByRole('button', { name: /mark done/i })
      .click();
    await expect(
      pageB.locator('#actions-list article.action.done').filter({ hasText: next }),
    ).toBeVisible({ timeout: 1000 });
  } finally {
    await close();
  }
});
