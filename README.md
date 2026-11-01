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

Scheduling is off on installation. Enabling it starts a round immediately, then waits the configured interval after the round finishes. Manual and scheduled rounds never overlap. Changing settings affects the next round. Closing the page has no effect on the backend timer.

Stop prevents new accounts and further disable actions. Already-submitted detector jobs finish naturally; no global cancel call is made. Restart marks unfinished rounds interrupted and never replays old disable intents. Scheduled operation resumes after a fresh interval when previously enabled.

## Build And Test

Requirements: Go 1.26+, Node.js/npm, and Docker with `golang:1.26-bookworm` for the Linux CGO shared library.

```powershell
npm ci --ignore-scripts
npm run build
go test ./...
docker run --rm --mount "type=bind,source=$PWD,target=/src" -w /src golang:1.26-bookworm sh -c 'CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -trimpath -o dist/cpa-codex-candy-guard-v0.1.0.so ./cmd/plugin'
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

Manual rollback: stop local CPA, restore the backup's `config.yaml` and `docker-compose.yml`, remove only this guard's `.so` and `.candy-guard.env`, and recreate the CPA service with its restored Compose file. Keep the guard state for diagnosis. Do not restore whole credential directories or overwrite other plugins' newly completed histories.

## API And Storage

All APIs are protected by CPA management authentication, under `/v0/management/plugins/cpa-codex-candy-guard`:

- `GET /state`: settings, accounts/models, current progress, next run, dependency readiness and last 20 round summaries.
- `PUT /settings`: `model`, `per_credential_concurrency`, `credential_concurrency`, `interval_minutes`, `schedule_enabled`, `scope` (`all_enabled` or `selected`), `credential_ids`.
- `POST /run`: start one manual round, or return 409 while already running.
- `POST /stop`: stop queueing and future actions for the active round.

The resource page is `/v0/resource/plugins/cpa-codex-candy-guard/ui`. Settings and summaries use an atomic JSON file at `plugins/cpa-codex-candy-guard-state.json`. No tokens, management keys or full model answers are stored there. Failed persistence blocks new disable actions. Unknown detector versions or schema changes pause operation instead of inferring outcomes. A healthy dependency allows manual retry after a pause.

The backend only permits literal loopback CPA URLs (`CPA_CANDY_GUARD_BASE_URL`, default `http://127.0.0.1:8317`), disables environment proxies and does not follow redirects. The management key is supplied through `CPA_CANDY_GUARD_MANAGEMENT_KEY`. Changing the key environment requires recreating CPA.

Source repository: [tedcy/cpa-codex-candy-guard](https://github.com/tedcy/cpa-codex-candy-guard). The plugin metadata points to this repository.

## Attribution

The ABI layout, CPAMC/CPAMP secure-storage compatibility, model alias rules and detector contract follow [CPA](https://github.com/router-for-me/CLIProxyAPI) and [Codex Candy Eval](https://github.com/haowang02/cpa-plugin-codex-candy-eval), both MIT-licensed. No detection algorithms or reference-bank assets are bundled. Icons are from Lucide, ISC-licensed; the generated UI bundle retains its license notice. See `THIRD_PARTY.md`.
