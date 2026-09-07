import * as path from 'node:path';
import { defineConfig, devices } from '@playwright/test';

const port = process.env.E2E_PORT || '8082';
// One webServer serves every worker from a single SQLite file, so the
// suite runs serially: concurrent workers would share tables through
// one server process and flake on each other's rows.
const dbPath =
  process.env.DB_PATH ||
  path.resolve(__dirname, '..', 'data', 'hindsight-e2e.db');
process.env.DB_PATH = dbPath;

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 2 : 0,
  reporter: 'list',
  use: {
    baseURL: `http://localhost:${port}`,
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: 'CGO_ENABLED=0 go build -trimpath -o hindsight . && ./hindsight',
    url: `http://localhost:${port}/healthz`,
    reuseExistingServer: false,
    cwd: path.resolve(__dirname, '..'),
    env: {
      PORT: port,
      DB_PATH: dbPath,
    },
  },
});
