import { test, expect } from '@playwright/test';
import { pickTemplate, boardIdFromUrl, setCardsHidden, waitForLive, newContext } from './helpers';

// Blind collection: while the board hides cards, a participant sees
// only placeholders (never bodies, authors, or counts) while the
// facilitator keeps full bodies — including cards added mid-hiding,
// which arrive redacted for one viewer and full for the other.
// Hiding has no product route (only reveal does), so the spec seeds
// the flag with the shared setCardsHidden helper, then reloads both
// viewers.

test('blind collect hides bodies from participants, not the facilitator', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const openSecret = `open secret ${stamp}`;
  const blindSecret = `blind secret ${stamp}`;

  const ctxA = await newContext(browser);
  const ctxB = await newContext(browser);
  const pageA = await ctxA.newPage();
  const pageB = await ctxB.newPage();

  try {
    await pageA.goto('/new');
    await pageA.locator('input[name="name"]').fill(`Blind e2e ${stamp}`);
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

    const colA = await pageA.locator('section.column').nth(0).getAttribute('id');
    await pageA.locator(`#${colA} input[name="body"]`).fill(openSecret);
    await pageA.locator(`#${colA} > form button[type="submit"]`).click();
    await expect(pageB.locator(`#${colA}`).getByText(openSecret)).toBeVisible({
      timeout: 8000,
    });

    setCardsHidden(bid, true);
    await pageA.reload();
    await pageB.reload();
    await expect(pageA.locator('#board-columns')).toBeVisible();
    await expect(pageB.locator('#board-columns')).toBeVisible();

    // Participant: placeholders only, zero bodies anywhere.
    const bodiesB = await pageB.locator('.card-body').allTextContents();
    expect(bodiesB.length).toBeGreaterThan(0);
    for (const text of bodiesB) {
      expect(text.trim()).toBe('•••');
    }
    expect(await pageB.content()).not.toContain(openSecret);

    // Facilitator: the same board, full bodies.
    await expect(
      pageA.locator('#board-columns').getByText(openSecret),
    ).toBeVisible();

    // A card added while hidden arrives redacted for the participant
    // and full for the facilitator through the live stream.
    await pageB.locator(`#${colA} input[name="body"]`).fill(blindSecret);
    await pageB.locator(`#${colA} > form button[type="submit"]`).click();
    await expect(pageA.locator(`#${colA}`).getByText(blindSecret)).toBeVisible({
      timeout: 8000,
    });
    const afterB = await pageB.locator('.card-body').allTextContents();
    expect(afterB.length).toBeGreaterThan(0);
    for (const text of afterB) {
      expect(text.trim()).toBe('•••');
    }
    expect(await pageB.content()).not.toContain(blindSecret);
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
