# Browser tests (Playwright)

Checks the three pages (order, kitchen, board) in Chromium against a locally
built v2 app, including that the board's EventSource fires `shutdown` when the
app gets SIGTERM.

## Prerequisites

- Node.js 22 or newer, and the test database: `eval "$(scripts/test-db.sh)"` from `01/`.
- `corepack npm install` (or `npm install`) in this directory. Versions are pinned in `package.json`.
- `npx playwright install chromium` downloads the browser.
- `sudo npx playwright install-deps` installs the system libraries Chromium
  needs, including a font package (without any font, headless Chromium
  crashes on `<input>`). On a host where you cannot use sudo, extract the
  packages into a private directory and set `LD_LIBRARY_PATH` and
  `FONTCONFIG_FILE`.

## Run

```
experiments/browser/run.sh
```

It clones `koupa_template` into a throwaway database, builds `bin/koupa`
(v2), starts it on a free port, runs the tests and drops the database. The
shutdown test starts a second instance on its own free port.
