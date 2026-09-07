import { test, expect } from '@playwright/test';
import { pickTemplate, newContext } from './helpers';

// Two viewers, one thread: a comment posted in context A must render
// inside the card fragment in context B through the live stream within
// a second, with the visible comment count advancing alongside it.
test('comment posted in A appears live in B with its count', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const cardBody = `threaded card ${stamp}`;
  const remark = `live remark ${stamp}`;

  const ctxA = await newContext(browser);
  const ctxB = await newContext(browser);
  const pageA = await ctxA.newPage();
  const pageB = await ctxB.newPage();

  try {
    await pageA.goto('/new');
    await pageA.locator('input[name="name"]').fill(`Comments e2e ${stamp}`);
    await pageA.locator('input[name="display_name"]').fill('Facilitator');
    await pickTemplate(pageA, 'mad-sad-glad');
    await pageA.getByRole('button', { name: /create board/i }).click();
    await expect(pageA).toHaveURL(/\/b\//);
    const bid = pageA.url().split('/b/')[1].split('?')[0];

    await pageB.goto(`/b/${bid}`);
    await expect(pageB.locator('#join-gate')).toBeVisible();
    await pageB.locator('#join-gate input[name="name"]').fill('Watcher');
    await pageB.locator('#join-gate button[type="submit"]').click();
    await expect(pageB.locator('#board-columns')).toBeVisible();

    const colA = await pageA.locator('section.column').nth(0).getAttribute('id');
    await pageA.locator(`#${colA} input[name="body"]`).fill(cardBody);
    await pageA.locator(`#${colA} > form button[type="submit"]`).click();
    const cardNode = pageA.locator(`#${colA} article.card`);
    await expect(cardNode).toHaveCount(1, { timeout: 8000 });
    const cardId = (await cardNode.getAttribute('id')) as string;
    expect(cardId).toMatch(/^card-\d+$/);
    await expect(pageB.locator(`#${cardId}`)).toBeVisible({ timeout: 8000 });

    // A posts through the card's own comment form; B observes the
    // thread and its count through the card-updated broadcast.
    await pageA.locator(`#${cardId} .comment-form input[name="comment"]`).fill(remark);
    await pageA.locator(`#${cardId} .comment-form button[type="submit"]`).click();
    await expect(pageB.locator(`#${cardId} .comment-body`).getByText(remark)).toBeVisible({
      timeout: 1000,
    });
    await expect(pageB.locator(`#${cardId} .comments`).getByText('1 comment')).toBeVisible({
      timeout: 1000,
    });

    // A's own fragment converges on the same thread, not a duplicate.
    await expect(pageA.locator(`#${cardId} .comment-body`)).toHaveCount(1, {
      timeout: 1000,
    });
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
