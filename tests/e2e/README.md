# Live end-to-end gate

Validates a PR against real WhatsApp: two bridges (accounts A and B) message each other through the REST API, and `e2e.py` asserts on what each side stores. Run it with `./run.sh` from this directory (`./run.sh offline` for tier 0 only).

## Tiers

- **Tier 0, offline** (no accounts needed): Go build + vet + test, Python unit tests, compose refuses to start without a token, image builds.
- **Tier 1, live**: starts both bridges, then runs `e2e.py` (text, reactions, edit, presence, read/unread/archive, media, groups, restart/re-auth). Messages carry a per-run tag (`e2e-<id>`) and only the two configured accounts are touched.

Tier 0 also runs in GitHub Actions (`.github/workflows/ci.yml`, minus the docker image build). Tier 1 cannot: it needs paired sessions.

## Modes

- **docker** (default): bridges run from `docker-compose.e2e.yml`; Go tests run in a `golang` container.
- **native** (`E2E_MODE=native`, env var or `.env` key): bridges run as plain processes via `native.sh`, Go tests run on the host. Use it where Docker is unavailable (this is how the PR monitor runs the gate).

## Setup

`run.sh` creates `.env` on first run and exits; fill in the phone numbers and re-run. Keys (names only):

`E2E_TOKEN` (generated), `E2E_A_PORT`, `E2E_B_PORT`, `E2E_A_PHONE`, `E2E_B_PHONE`, optionally `E2E_MODE`.

Pair each account once: start the bridges, open `http://127.0.0.1:<E2E_A_PORT>/qr` and `.../<E2E_B_PORT>/qr`, and scan with WhatsApp (Linked Devices). Sessions persist in `.data/`, so this is a one-time step. Re-scanning forces a history re-sync; avoid it.

## Why things are gitignored

`.env` (token, real numbers), `.data/` (session databases, equivalent to being logged in), `.media/` and `report-*.md` (may contain numbers and message text) stay local. Never commit them.

## Reading the report

`run.sh` writes `report-<date>.md` and prints it. Each line is `PASS` or `FAIL`; a failure includes the last 25 lines of output. The final `Result` is `PASS` only if every step passed. For tier 1 the unittest summary is in the fenced block: `OK`, plus counts for skipped and expected failures.

## Known gaps

- `@unittest.expectedFailure` marks a known bridge gap (currently: incoming edits are not applied to the stored message). When the gap is fixed the test flips to "unexpected success", which fails the run: remove the decorator then.
- Tests are ordered (`test_NN`) and share ids; if one fails, dependents report `skipped` ("prerequisite not available"), which hides their coverage for that run.
- App-state calls (read/unread/archive) sometimes get a transient server-side 500 from WhatsApp; they are retried, but persistent failures still fail the test.
