import { test, expect } from '@playwright/test';
import { pickTemplate, newContext } from './helpers';

// Two viewers, one board: everything A does to cards (add, move, edit)
// must show up in B through the live stream within a second.
test('card add, move and edit sync from A to B in under a second', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const body = `streamed card ${stamp}`;

  const ctxA = await newContext(browser);
  const ctxB = await newContext(browser);
  const pageA = await ctxA.newPage();
  const pageB = await ctxB.newPage();

  try {
    // A creates the board and lands on it as facilitator + participant.
    await pageA.goto('/new');
    await pageA.locator('input[name="name"]').fill(`Cards e2e ${stamp}`);
    await pageA.locator('input[name="display_name"]').fill('Facilitator');
    await pickTemplate(pageA, 'mad-sad-glad');
    await pageA.getByRole('button', { name: /create board/i }).click();
    await expect(pageA).toHaveURL(/\/b\//);
    const bid = pageA.url().split('/b/')[1].split('?')[0];

    // B joins through the share link.
    await pageB.goto(`/b/${bid}`);
    await expect(pageB.locator('#join-gate')).toBeVisible();
    await pageB.locator('#join-gate input[name="name"]').fill('Watcher');
    await pageB.locator('#join-gate button[type="submit"]').click();
    await expect(pageB.locator('#board-columns')).toBeVisible();

    // Column ids are identical in both contexts.
    const colA = await pageA.locator('section.column').nth(0).getAttribute('id');
    const colB = await pageA.locator('section.column').nth(1).getAttribute('id');
    expect(colA).toMatch(/^column-\d+$/);
    expect(colB).toMatch(/^column-\d+$/);

    // ADD in A through the column form; B observes it through the stream.
    await pageA.locator(`#${colA} input[name="body"]`).fill(body);
    await pageA.locator(`#${colA} > form button[type="submit"]`).click();
    await expect(pageB.locator(`#${colA}`).getByText(body)).toBeVisible({
      timeout: 1000,
    });

    // A converges on exactly one card node: its own morph and the
    // broadcast morph meet by node id in either arrival order.
    const cardNode = pageA.locator(`#${colA} article.card`);
    await expect(cardNode).toHaveCount(1, { timeout: 1000 });
    await expect(cardNode.getByText(body)).toBeVisible({ timeout: 1000 });
    const cid = ((await cardNode.getAttribute('id')) as string).replace(
      'card-',
      '',
    );
    expect(cid).toMatch(/^\d+$/);
    const destColumnId = (colB as string).replace('column-', '');

    // MOVE in A through the drop contract; B sees the card leave and land.
    const moveStatus = await pageA.evaluate(
      async ({ cid, columnId }) => {
        const params = new URLSearchParams({
          column_id: columnId,
          position: '0',
        });
        const res = await fetch(`/cards/${cid}`, {
          method: 'PUT',
          body: params,
        });
        return res.status;
      },
      { cid, columnId: destColumnId },
    );
    expect(moveStatus).toBe(200);
    await expect(pageB.locator(`#${colB}`).getByText(body)).toBeVisible({
      timeout: 1000,
    });
    await expect(
      pageB.locator(`#${colA}`).getByText(body),
    ).toHaveCount(0, { timeout: 1000 });

    // EDIT in A; B sees the new text in place of the old.
    const edited = `${body} edited`;
    const editStatus = await pageA.evaluate(
      async ({ cid, text }) => {
        const res = await fetch(`/cards/${cid}`, {
          method: 'PUT',
          body: new URLSearchParams({ body: text }),
        });
        return res.status;
      },
      { cid, text: edited },
    );
    expect(editStatus).toBe(200);
    await expect(pageB.locator(`#${colB}`).getByText(edited)).toBeVisible({
      timeout: 1000,
    });
    await expect(
      pageB.locator(`#${colB}`).getByText(body, { exact: true }),
    ).toHaveCount(0, { timeout: 1000 });
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
