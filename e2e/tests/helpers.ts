import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { DatabaseSync } from 'node:sqlite';
import * as path from 'node:path';

// Every e2e context dials the server from the same loopback peer, and
// the mutation limiter buckets per client key: sharing one bucket means
// one spec's mutations drain it for the whole serial suite and later
// specs fail on silent 429s. clientKey prefers a well-formed forwarded
// address over the peer, so each context pins a unique documentation
// range (198.51.100.0/24) address of its own.
let clientSeq = 0;

export function newContext(browser: Browser): Promise<BrowserContext> {
  clientSeq += 1;
  return browser.newContext({
    extraHTTPHeaders: {
      'X-Forwarded-For': `198.51.100.${(clientSeq % 250) + 1}`,
    },
  });
}

export async function pickTemplate(page: Page, key: string): Promise<void> {
  await page.locator(`label:has(input[value="${key}"])`).click();
}

export function boardIdFromUrl(url: string): string {
  const match = new URL(url).pathname.match(/\/b\/([^/?#]+)/);
  if (!match) {
    throw new Error(`cannot parse board id from ${url}`);
  }
  return match[1];
}

// Cross-viewer live assertions need the viewer's SSE stream open before
// the other side acts: a viewer that connects late gets sync-required,
// which heals columns but not the kudos/actions walls. The connection
// badge flips to data-connected="true" on htmx:sseOpen.
export async function waitForLive(page: Page, timeout = 5000): Promise<void> {
  await expect(page.locator('#connection-badge[data-connected="true"]')).toBeVisible({ timeout });
}

export async function createBoard(
  page: Page,
  opts: { name: string; template?: string; displayName?: string },
): Promise<string> {
  await page.goto('/new');
  await page.locator('input[name="name"]').fill(opts.name);
  await page.locator('input[name="display_name"]').fill(opts.displayName ?? 'Facilitator');
  if (opts.template) {
    await pickTemplate(page, opts.template);
  }
  await page.getByRole('button', { name: /create board/i }).click();
  await expect(page).toHaveURL(/\/b\//);
  return boardIdFromUrl(page.url());
}

export async function joinBoard(
  page: Page,
  bid: string,
  name: string,
): Promise<void> {
  await page.goto(`/b/${bid}`);
  await expect(page.locator('#join-gate')).toBeVisible();
  await page.locator('#join-gate input[name="name"]').fill(name);
  await page.locator('#join-gate button[type="submit"]').click();
  await expect(page.locator('#board-columns')).toBeVisible();
  // Joining swaps the columns fragment into the gate page. Reload so
  // the viewer gets the full board shell (phase rail, timer, export),
  // then assert the shell actually arrived.
  await page.goto(`/b/${bid}`);
  await expect(page.locator('#board-columns')).toBeVisible();
  await expect(page.locator('#phase-badge')).toBeVisible();
  await expect(page.locator('#timer')).toBeVisible();
  await expect(page.locator('#export-section')).toBeVisible();
}

// Hiding cards has no product route by design (only reveal does — see
// db/facilitation.go SetCardsHidden): hiding is seeded by tests and
// tooling. Write the flag straight into the shared test database with
// the built-in node:sqlite driver (no sqlite3 binary needed), with a
// busy timeout so the write waits out the running server's lock.
export function setCardsHidden(publicId: string, hidden: boolean): void {
  if (!/^[A-Za-z0-9_-]+$/.test(publicId)) {
    throw new Error(`refusing to interpolate suspicious board id ${publicId}`);
  }
  const dbPath =
    process.env.DB_PATH ||
    path.resolve(__dirname, '..', '..', 'data', 'hindsight-e2e.db');
  const db = new DatabaseSync(dbPath);
  try {
    db.exec('PRAGMA busy_timeout = 5000');
    const result = db
      .prepare('UPDATE boards SET cards_hidden = ? WHERE public_id = ?')
      .run(hidden ? 1 : 0, publicId);
    if (Number(result.changes) !== 1) {
      throw new Error(`setCardsHidden updated ${String(result.changes)} rows for ${publicId}`);
    }
  } finally {
    db.close();
  }
}

export async function twoViewers(browser: Browser): Promise<{
  ctxA: BrowserContext;
  ctxB: BrowserContext;
  pageA: Page;
  pageB: Page;
  close: () => Promise<void>;
}> {
  const ctxA = await newContext(browser);
  try {
    const ctxB = await newContext(browser);
    const pageA = await ctxA.newPage();
    const pageB = await ctxB.newPage();
    return {
      ctxA,
      ctxB,
      pageA,
      pageB,
      close: async () => {
        await ctxA.close();
        await ctxB.close();
      },
    };
  } catch (err) {
    await ctxA.close();
    throw err;
  }
}
