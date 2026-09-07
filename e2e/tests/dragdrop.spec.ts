import { test, expect, Page } from '@playwright/test';
import { pickTemplate, boardIdFromUrl, waitForLive, newContext } from './helpers';

// Drag-and-drop is HTML5 DnD with one PUT per drop. True OS-level DnD
// is not exercised here: synthetic drag events drive the real
// bindings (the handlers tolerate a missing dataTransfer, which
// synthetic events cannot carry); the drop issues the same PUT a
// physical drag would, and the specs assert that PUT contract plus
// two-viewer convergence.
async function dragStart(page: Page, cardId: string) {
  await page.evaluate((cid) => {
    const node = document.getElementById(`card-${cid}`);
    if (!node) throw new Error(`no card-${cid}`);
    node.dispatchEvent(new DragEvent('dragstart', { bubbles: true, cancelable: true }));
  }, cardId);
}

async function dragOverDrop(page: Page, destColumnId: string) {
  await page.evaluate((col) => {
    const list = document.querySelector(`#${col} .card-list`);
    if (!list) throw new Error(`no card list in ${col}`);
    const rect = list.getBoundingClientRect();
    const init = {
      bubbles: true,
      cancelable: true,
      clientX: rect.left + 10,
      clientY: rect.bottom - 10,
    };
    list.dispatchEvent(new DragEvent('dragover', init));
    list.dispatchEvent(new DragEvent('drop', init));
  }, destColumnId);
}

async function columnBodies(page: Page, columnId: string): Promise<string[]> {
  return page.locator(`#${columnId} .card-body`).allTextContents();
}

// Fill the column form and click Add, retrying while the click lands
// inside a settling morph and issues no request (the button under the
// cursor is replaced between press and release, so nothing submits).
async function addCard(page: Page, columnId: string, text: string) {
  for (let attempt = 0; attempt < 3; attempt++) {
    await page.locator(`#${columnId} input[name="body"]`).fill(text);
    const submitted = page
      .waitForRequest(
        (req) => req.method() === 'POST' && req.url().includes('/cards'),
        { timeout: 2000 },
      )
      .then(
        () => true,
        () => false,
      );
    await page.locator(`#${columnId} > form button[type="submit"]`).click();
    if (await submitted) return;
  }
  throw new Error(`add card gave up: ${text}`);
}

async function setupBoard(browser: import('@playwright/test').Browser, stamp: string) {
  const ctxA = await newContext(browser);
  const ctxB = await newContext(browser);
  const pageA = await ctxA.newPage();
  const pageB = await ctxB.newPage();

  await pageA.goto('/new');
  await pageA.locator('input[name="name"]').fill(`DnD e2e ${stamp}`);
  await pageA.locator('input[name="display_name"]').fill('Facilitator');
  await pickTemplate(pageA, 'mad-sad-glad');
  await pageA.getByRole('button', { name: /create board/i }).click();
  await expect(pageA).toHaveURL(/\/b\//);
  const bid = boardIdFromUrl(pageA.url());

  await pageB.goto(`/b/${bid}`);
  await pageB.locator('#join-gate input[name="name"]').fill('Watcher');
  await pageB.locator('#join-gate button[type="submit"]').click();
  await expect(pageB.locator('#board-columns')).toBeVisible();
  await waitForLive(pageA);
  await waitForLive(pageB);

  return { ctxA, ctxB, pageA, pageB, bid };
}

test('drag moves a card across columns with a single PUT', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const body = `dragged card ${stamp}`;
  const { ctxA, ctxB, pageA, pageB } = await setupBoard(browser, stamp);

  const puts: string[] = [];
  pageA.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().includes('/cards/')) {
      puts.push(`${req.url()}?${req.postData() ?? ''}`);
    }
  });

  try {
    const colA = (await pageA
      .locator('section.column')
      .nth(0)
      .getAttribute('id')) as string;
    const colB = (await pageA
      .locator('section.column')
      .nth(1)
      .getAttribute('id')) as string;

    await addCard(pageA, colA, body);
    await expect(pageB.locator(`#${colA}`).getByText(body)).toBeVisible({
      timeout: 8000,
    });

    const cid = (
      (await pageA
        .locator(`#${colA} article.card`)
        .getAttribute('id')) as string
    ).replace('card-', '');

    const dropResponse = pageA.waitForResponse(
      (res) => res.request().method() === 'PUT' && res.url().includes('/cards/'),
    );
    await dragStart(pageA, cid);
    await dragOverDrop(pageA, colB);
    const res = await dropResponse;
    expect(res.status()).toBe(200);

    // Exactly one drop PUT, carrying the pre-drag slot for the
    // server-side staleness check.
    expect(puts).toHaveLength(1);
    expect(puts[0]).toContain('from_column_id=');
    expect(puts[0]).toContain('from_position=');

    // Both viewers converge: the card leaves and lands.
    await expect(pageB.locator(`#${colB}`).getByText(body)).toBeVisible({
      timeout: 8000,
    });
    await expect(pageB.locator(`#${colA}`).getByText(body)).toHaveCount(0, {
      timeout: 8000,
    });
    await expect(pageA.locator(`#${colB}`).getByText(body)).toBeVisible({
      timeout: 8000,
    });
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});

test('remote update mid-drag snaps back without a PUT', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const anchor = `anchor ${stamp}`;
  const draggable = `draggable ${stamp}`;
  const remote = `remote ${stamp}`;
  const { ctxA, ctxB, pageA, pageB } = await setupBoard(browser, stamp);

  let puts = 0;
  pageA.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().includes('/cards/')) {
      puts += 1;
    }
  });

  try {
    const colA = (await pageA
      .locator('section.column')
      .nth(0)
      .getAttribute('id')) as string;
    const colB = (await pageA
      .locator('section.column')
      .nth(1)
      .getAttribute('id')) as string;

    for (const text of [anchor, draggable]) {
      await addCard(pageA, colA, text);
      await expect(pageB.locator(`#${colA}`).getByText(text)).toBeVisible({
        timeout: 8000,
      });
    }
    const cid = (
      (await pageA
        .locator(`#${colA} article.card`, { hasText: draggable })
        .getAttribute('id')) as string
    ).replace('card-', '');

    // The drag starts; then a remote card lands in the same column,
    // swapping it out from under the gesture.
    await dragStart(pageA, cid);
    await addCard(pageB, colA, remote);
    await expect(pageA.locator(`#${colA}`).getByText(remote)).toBeVisible({
      timeout: 8000,
    });

    // Dropping now must snap back: no PUT, a retry hint, and the
    // server order (anchor, draggable, remote) intact. The snap-back
    // refetches the columns root, which must replace in place — never
    // nest a duplicate root (and stream).
    await dragOverDrop(pageA, colB);
    await expect(pageA.locator('#flash').getByText(/while you were dragging/)).toBeVisible({
      timeout: 8000,
    });
    expect(puts).toBe(0);
    expect(
      await pageA.evaluate(() => document.querySelectorAll('#board-columns').length),
    ).toBe(1);
    expect(await columnBodies(pageA, colA)).toEqual([anchor, draggable, remote]);
    expect(await columnBodies(pageB, colA)).toEqual([anchor, draggable, remote]);
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
