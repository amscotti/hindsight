import { test, expect } from '@playwright/test';
import { pickTemplate, boardIdFromUrl, setCardsHidden, waitForLive, newContext } from './helpers';

// Full facilitation loop across two viewers: blind collect -> reveal ->
// vote -> sort -> group -> timed spotlight discussion -> done. The
// facilitator (A) drives every control through the panel UI; the
// participant (B) follows everything through the live stream. Hiding
// has no product route (only reveal does), so the blind-collect step
// seeds the flag with the shared setCardsHidden helper; the reveal
// step drives the real facilitator panel control.

test('facilitation loop runs end to end in both contexts', async ({
  browser,
}) => {
  const stamp = Date.now().toString(36);
  const firstBody = `loop first ${stamp}`;
  const secondBody = `loop second ${stamp}`;

  const ctxA = await newContext(browser);
  const ctxB = await newContext(browser);
  const pageA = await ctxA.newPage();
  const pageB = await ctxB.newPage();

  try {
    await pageA.goto('/new');
    await pageA.locator('input[name="name"]').fill(`Loop e2e ${stamp}`);
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

    // Only the facilitator gets the control panel.
    await expect(pageA.locator('#facilitator-panel')).toBeAttached();
    await expect(pageB.locator('#facilitator-panel')).toHaveCount(0);

    const colA = await pageA.locator('section.column').nth(0).getAttribute('id');
    expect(colA).toMatch(/^column-\d+$/);

    // Untouched columns keep their empty-state invitation.
    await expect(
      pageA
        .locator('section.column')
        .nth(1)
        .getByText(/no cards yet/i),
    ).toBeVisible();

    // Open collect: A adds, B sees it live.
    await pageA.locator(`#${colA} input[name="body"]`).fill(firstBody);
    await pageA.locator(`#${colA} > form button[type="submit"]`).click();
    await expect(pageB.locator(`#${colA}`).getByText(firstBody)).toBeVisible({
      timeout: 8000,
    });
    const cid1 = (
      (await pageA
        .locator(`#${colA} article.card`, { hasText: firstBody })
        .getAttribute('id')) as string
    ).replace('card-', '');
    expect(cid1).toMatch(/^\d+$/);

    // Blind collect: B sees placeholders, A keeps full bodies.
    setCardsHidden(bid, true);
    await pageA.reload();
    await pageB.reload();
    await expect(pageB.locator('#board-columns')).toBeVisible();
    const bodiesB = await pageB.locator('.card-body').allTextContents();
    expect(bodiesB.length).toBeGreaterThan(0);
    for (const text of bodiesB) {
      expect(text.trim()).toBe('•••');
    }
    await expect(
      pageA.locator('#board-columns').getByText(firstBody),
    ).toBeVisible();

    // Reveal through the panel: B refetches into full bodies.
    await pageA.getByRole('button', { name: /facilitator controls/i }).click();
    await pageA.getByRole('button', { name: /reveal cards/i }).click();
    await expect(pageB.locator(`#${colA}`).getByText(firstBody)).toBeVisible({
      timeout: 8000,
    });

    // Vote phase: the badge follows in both contexts.
    await pageA
      .locator('#facilitator-panel button[name="phase"][value="vote"]')
      .click();
    await expect(pageB.locator('#phase-badge')).toContainText('Phase: Vote', {
      timeout: 8000,
    });
    await expect(pageA.locator('#phase-badge')).toContainText('Phase: Vote');

    // A second card, then votes: B backs the first, both back the second.
    await pageA.locator(`#${colA} input[name="body"]`).fill(secondBody);
    await pageA.locator(`#${colA} > form button[type="submit"]`).click();
    await expect(pageB.locator(`#${colA}`).getByText(secondBody)).toBeVisible({
      timeout: 8000,
    });
    const cid2 = (
      (await pageA
        .locator(`#${colA} article.card`, { hasText: secondBody })
        .getAttribute('id')) as string
    ).replace('card-', '');
    await pageB
      .locator(`#${colA} article.card`, { hasText: firstBody })
      .getByRole('button', { name: /^vote$/i })
      .click();
    await pageA
      .locator(`#${colA} article.card`, { hasText: secondBody })
      .getByRole('button', { name: /^vote$/i })
      .click();
    await pageB
      .locator(`#${colA} article.card`, { hasText: secondBody })
      .getByRole('button', { name: /^vote$/i })
      .click();
    await expect(
      pageA.locator(`#${colA} article.card`, { hasText: secondBody }),
    ).toContainText('2 votes', { timeout: 8000 });

    // Sort by votes: the twice-backed card leads in both contexts. The
    // reorder arrives through the column broadcast, so poll until both
    // viewers converge instead of reading mid-flight.
    await pageA
      .getByRole('button', { name: /^mad$/i })
      .click();
    for (const page of [pageA, pageB]) {
      await expect(async () => {
        const order = await page
          .locator(`#${colA} .card-body`)
          .allTextContents();
        expect(order.slice(0, 2)).toEqual([secondBody, firstBody]);
      }).toPass({ timeout: 8000 });
    }

    // Group the first card under the second: nested render plus the sum.
    await pageA.locator('#group-form input[name="card_id"]').fill(cid1);
    await pageA.locator('#group-form input[name="leader_id"]').fill(cid2);
    await pageA.getByRole('button', { name: /group cards/i }).click();
    for (const page of [pageA, pageB]) {
      const leader = page.locator(`#card-${cid2}`);
      await expect(leader.getByText(firstBody)).toBeVisible({ timeout: 8000 });
      await expect(leader).toContainText('3 votes');
    }

    // Timer: both viewers render the countdown.
    await pageA.locator('#facilitator-panel input[name="seconds"][type="number"]').fill('120');
    await pageA.getByRole('button', { name: /^start$/i }).click();
    for (const page of [pageA, pageB]) {
      await expect(page.locator('#timer')).toContainText(/\d+:\d+ left/, {
        timeout: 8000,
      });
    }

    // Spotlight: viewers follow the focus queue; advancing marks discussed.
    await pageA.getByRole('button', { name: /^next card$/i }).click();
    for (const page of [pageA, pageB]) {
      await expect(page.locator('#spotlight')).toBeVisible({ timeout: 8000 });
      await expect(page.locator('#spotlight-label')).toContainText(
        `Card ${cid2}`,
      );
    }
    await pageA.getByRole('button', { name: /^next card$/i }).click();
    for (const page of [pageA, pageB]) {
      await expect(
        page.locator(`#card-${cid2}`).getByText(/discussed/i),
      ).toBeVisible({ timeout: 8000 });
    }

    // Done: the loop closes in both contexts.
    await pageA
      .locator('#facilitator-panel button[name="phase"][value="done"]')
      .click();
    for (const page of [pageA, pageB]) {
      await expect(page.locator('#phase-badge')).toContainText('Phase: Done', {
        timeout: 8000,
      });
    }
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
