# Codex Candy Guard

A local CPA management plugin that queues fingerprint tests through **Codex Candy Eval v0.4.4** and disables credentials according to the selected policy. It does not run its own model detector, replace CPA routing, or modify the account-concurrency plugin.

## Policy

- Only enabled, available Codex OAuth files are eligible. API-key providers are excluded.
- Completed and partial detections with a nonempty prediction use Candy Eval's exact model aliases and comparison. A mismatch disables the entire credential without an extra probability threshold or confirmation test.
- A final error or sample error containing the exact server-overload message or a structured `server_is_overloaded` code also disables it. Recovered intermediate errors are not available in final records and do not trigger closure.
- Failed requests, HTTP 503 by itself, cancellation, missing predictions and `not_model` results otherwise do not disable accounts.
- A result must match this worker's credential, requested model, submission time and a unique new record ID. Old, ambiguous or unreadable results never authorize an action.
- Disable uses CPA's status API with the original name and auth index, checks identity again and verifies the resulting state. No credential JSON is rewritten by this plugin.
- Disabled credentials are never automatically restored or periodically retested.

## Controls

The CPA sidebar menu is **Codex 自动巡检**. Choose a model, per-credential concurrency (1–3), simultaneous credential count (default 2), and interval in minutes (default 10). All enabled accounts are selected by default; a selected subset can be saved instead.

The original detector still caps real fingerprint requests at six across the entire CPA process, including tests started elsewhere. The guard's global setting controls credentials, not that request cap.

The mode selector offers normal, scheduled, and second-check operation. Scheduling and second checks are mutually exclusive and off on installation. Selecting or saving a mode does not start detection; use Run Now. Scheduled operation waits the configured interval after each completed round before starting the next one. Manual and scheduled rounds never overlap. Changing settings affects the next round. Closing the page has no effect on backend tasks.

Second-check mode adds at most one follow-up per credential, with a shared positive-integer delay of 1–525600 minutes (default 10). Each delay begins when the guard observes that credential's first result, not when the entire batch finishes. First checks have priority over due follow-ups; waiting follow-ups consume no credential slots, and both types share the guard's credential-concurrency limit. Every check still runs the original detector's full fingerprint test and remains subject to its six-request cap.

Completed, partial, and ordinary failed first checks are eligible for a follow-up, including results with no attribution. A first-check mismatch or final overload still triggers immediate disabling and no follow-up. Unstarted, cancelled, ambiguous, and dependency-error checks do not schedule follow-ups. A follow-up revalidates identity, availability, model support and competing tests, skipping unavailable credentials rather than rescheduling them. This mode finishes after its follow-ups and never starts a third check or another round.

Stop immediately wakes the pending queue and prevents new checks and further disable actions. Already-submitted detector jobs finish naturally; no global cancel call is made. Restart marks unfinished rounds and waiting follow-ups interrupted and never replays delayed checks or old disable intents. Scheduled operation resumes after a fresh interval when previously enabled.

## Build And Test

Requirements: Go 1.26+, Node.js/npm, and Docker with `golang:1.26-bookworm` for the Linux CGO shared library.

```powershell
npm ci --ignore-scripts
npm run build
go test ./...
docker run --rm --mount "type=bind,source=$PWD,target=/src" -w /src golang:1.26-bookworm sh -c 'CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -trimpath -o dist/cpa-codex-candy-guard-v0.2.0.so ./cmd/plugin'
go build -o test-output/demo.exe ./cmd/demo
npx playwright test
```

The demo and browser tests use synthetic accounts only. They cannot contact or disable real CPA accounts. Browser tests use an existing Chrome installation.

## Local Deployment

`scripts/install_local.py` targets this workspace's local CPA on port 18317 only. It obtains the existing management key in memory from the local Keeper environment, writes a separate ignored `.candy-guard.env`, and adds that file to the CPA service's `env_file` list. The backend key is never returned to the browser or written to run state or logs.

```powershell
python scripts/install_local.py stage
python scripts/install_local.py install
python scripts/install_local.py verify
```

The installer requires the verified binary digest to match the isolated stage run. It waits up to ten minutes for existing detector jobs, preserves the other plugins/configuration, and keeps a private `install-backup-*` snapshot. It recreates only local CPA, verifies all three plugins, keeps scheduling off, and rolls back if verification fails. It never starts a real-account detection run.

For an existing installation, build the new binary, run `stage`, then run `python scripts/install_local.py update-binary`. This waits for idle tests and requires scheduling to be off, backs up configuration and plugin files/state, replaces only the guard's library, and restarts the same local CPA container. Verification checks that existing settings/history, the other plugins, and Keeper are preserved. A failed upgrade restores the previous library and guard state. Run `verify` after the update.

Manual rollback: stop local CPA, restore the backup's `config.yaml` and `docker-compose.yml`, remove only this guard's `.so` and `.candy-guard.env`, and recreate the CPA service with its restored Compose file. Keep the guard state for diagnosis. Do not restore whole credential directories or overwrite other plugins' newly completed histories.

## API And Storage

All APIs are protected by CPA management authentication, under `/v0/management/plugins/cpa-codex-candy-guard`:

- `GET /state`: settings, accounts/models, current progress, next run, dependency readiness and last 20 round summaries.
- `PUT /settings`: `model`, `per_credential_concurrency`, `credential_concurrency`, `interval_minutes`, `schedule_enabled`, `recheck_enabled`, `recheck_delay_minutes`, `scope` (`all_enabled` or `selected`), `credential_ids`. Enabling both modes is rejected; a missing recheck delay defaults to 10 for older clients, while an explicit zero is invalid.
- `POST /run`: start one manual round, or return 409 while already running.
- `POST /stop`: stop queueing and future actions for the active round.

The resource page is `/v0/resource/plugins/cpa-codex-candy-guard/ui`. Settings and summaries use an atomic JSON file at `plugins/cpa-codex-candy-guard-state.json`. No tokens, management keys or full model answers are stored there. Failed persistence blocks new disable actions. Unknown detector versions or schema changes pause operation instead of inferring outcomes. A healthy dependency allows manual retry after a pause.

Each history item identifies its credential and `check_index` (1 for a first check, 2 for a follow-up), with `due_at` for follow-ups. Legacy items without an index count as first checks. Each check correlates its own newly submitted record; a first-check record cannot authorize a follow-up action. `next_run` shows the earliest queued follow-up while a second-check round is active, or the next scheduled round when idle. Settings, mode, and concurrency are snapshotted for each round; history remains capped at 20 rounds.

The backend only permits literal loopback CPA URLs (`CPA_CANDY_GUARD_BASE_URL`, default `http://127.0.0.1:8317`), disables environment proxies and does not follow redirects. The management key is supplied through `CPA_CANDY_GUARD_MANAGEMENT_KEY`. Changing the key environment requires recreating CPA.

Source repository: [tedcy/cpa-codex-candy-guard](https://github.com/tedcy/cpa-codex-candy-guard). The plugin metadata points to this repository.

## Attribution

The ABI layout, CPAMC/CPAMP secure-storage compatibility, model alias rules and detector contract follow [CPA](https://github.com/router-for-me/CLIProxyAPI) and [Codex Candy Eval](https://github.com/haowang02/cpa-plugin-codex-candy-eval), both MIT-licensed. No detection algorithms or reference-bank assets are bundled. Icons are from Lucide, ISC-licensed; the generated UI bundle retains its license notice. See `THIRD_PARTY.md`.
