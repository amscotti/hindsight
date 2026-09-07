import { test, expect } from '@playwright/test';
import { createBoard, joinBoard, twoViewers, waitForLive } from './helpers';

test('vote syncs live between two viewers', async ({ browser }) => {
  const stamp = Date.now().toString(36);
  const body = `vote card ${stamp}`;
  const { pageA, pageB, close } = await twoViewers(browser);

  try {
    const bid = await createBoard(pageA, {
      name: `Votes e2e ${stamp}`,
      template: 'mad-sad-glad',
    });
    await joinBoard(pageB, bid, 'Watcher');
    await waitForLive(pageA);
    await waitForLive(pageB);

    const col = await pageA.locator('section.column').nth(0).getAttribute('id');
    await pageA.locator(`#${col} input[name="body"]`).fill(body);
    await pageA.locator(`#${col} > form button[type="submit"]`).click();
    const card = pageA.locator(`#${col} article.card`, { hasText: body });
    await expect(card).toBeVisible({ timeout: 8000 });
    await expect(pageB.locator(`#${col}`).getByText(body)).toBeVisible({
      timeout: 1000,
    });

    await pageA.getByRole('button', { name: /facilitator controls/i }).click();
    await pageA
      .locator('#facilitator-panel button[name="phase"][value="vote"]')
      .click();
    await expect(pageB.locator('#phase-badge')).toContainText('Phase: Vote', {
      timeout: 1000,
    });

    await pageB
      .locator(`#${col} article.card`, { hasText: body })
      .getByRole('button', { name: /^vote$/i })
      .click();
    await expect(
      pageA.locator(`#${col} article.card`, { hasText: body }),
    ).toContainText('1 vote', { timeout: 1000 });
    await expect(
      pageB.locator(`#${col} article.card`, { hasText: body }),
    ).toContainText('1 vote', { timeout: 1000 });
  } finally {
    await close();
  }
});
