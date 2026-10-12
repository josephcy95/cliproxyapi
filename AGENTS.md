# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex/Qoder compatible APIs with OAuth and round-robin load balancing.

## Repository
- **Origin (this fork, push/release):** https://github.com/josephcy95/CLIProxyAPI (`origin`)
- **Upstream:** https://github.com/router-for-me/CLIProxyAPI (`upstream`)
- Tags / releases are published on **josephcy95** only. Use `gh -R josephcy95/CLIProxyAPI` when default remote context is wrong.
- Docker image: `ghcr.io/josephcy95/cli-proxy-api` (`:latest` + version on `v*` tag workflows).
- Workspace sibling UI: `../cpa-management-center-forked` (Management Center). Parent workspace notes: `../AGENTS.md`.

## Fork policies

### Upstream sync (preserve this fork)
This fork exists for **custom behavior**. Upstream is for bug fixes and additive features — not to erase fork-only logic.

- Prefer a full merge of upstream release tags / `upstream/main` so the fork is not left “N commits behind”, but start it with `git merge --no-commit --no-ff upstream/<ref>`. Git applying a change cleanly does not authorize that change.
- `--no-commit` still applies every clean upstream change to the index; it only pauses before creating the commit. Automatic merges require the same semantic review as textual conflicts.
- Do not create the merge commit until every resulting API change has passed the fork-preservation gate below. After merge: `gofmt`, `go mod tidy` if needed, compile, run targeted tests, then ship only per ship policy.

### Upstream commit categories (mandatory)

Before retaining any upstream change, classify it into exactly one primary bucket. Record the bucket in the path/function ledger. This is the tie-breaker when “prefer upstream” and “preserve the fork” seem to conflict:

| Bucket | What it is | Default action |
|---|---|---|
| **1. Protocol / client tracking** | Mimicking real provider clients (Codex/Claude/Gemini/Antigravity/etc.): wire format, headers, payloads, auth handshake, retries, model lists, websocket/streaming quirks, API compatibility | **Prefer upstream.** Upstream tracks live client behaviour more closely than this fork. Take it as the base and re-apply fork hooks onto it. |
| **2. Features** | New product capability, QoL, architecture that is not required to mimic a client | **Merge if it does not conflict** with fork-owned behaviour. **Ask the user** if it looks useless, speculative, or conflicts with a fork custom feature. If the fork already has the same capability in a **superior** way, **keep the fork implementation** — you may learn from upstream to optimise ours, but never regress or replace ours with an inferior upstream version. |
| **3. Sponsor / promo** | Ads, sponsor badges, donate/paywall chrome, forced attribution, sponsor-only providers/routes, marketing splash, or code whose purpose is sponsorship/promotion | **Never merge.** Drop it entirely. Do not ask unless the user explicitly wants sponsor content. |

Tie-breaker in one line: **protocol → upstream wins; product/QoL feature → fork wins when equal or better; sponsor → always drop.**

### Sync intent: upstream protocol authority

When the user says “sync with upstream”, interpret it as a real merge whose **primary goal** is bucket 1 — provider/client protocol behaviour. Prefer upstream implementations for Codex/Claude/Gemini/Antigravity protocol emulation, headers, payload translation, retries, and API compatibility.

The fork is primarily for QoL and user-facing features, fork-specific configuration/deployment, management UI integration, providers not accepted upstream, and other deliberate product behaviour (bucket 2). Preserve those. Do **not** keep an older fork protocol implementation merely because it differs from upstream — for shared **protocol** code, use upstream as the base and deliberately re-apply only the fork-owned product behaviour that remains required.

For auth lifecycle, persistence, scheduling, and concurrency: if upstream’s change is **protocol-driven or clearly more robust architecture** (still bucket 1 / robust bucket 2), it may replace the fork implementation — port fork-specific hooks and policies into that architecture rather than reverting to older fork code. If the fork’s version is already superior for our product needs, keep ours and ask before swapping. Validate that Qoder/Qoder CN, Codex private-instructions, xAI/Codex failure policy, usage monitoring, management routes/assets, model-context overrides, and the single data-root behaviour survive.

### API fork-preservation gate (mandatory before committing an upstream merge)
1. Start from a clean worktree, record the pre-merge commit, fetch upstream, and perform a no-commit merge. Review the complete resulting diff against the recorded commit, including non-conflicting automatic changes, file deletions, renames, and splits.
2. For every changed fork-owned file or shared function, classify the incoming diff: allowed functional/security fix to retain, manually combined change, or upstream change excluded in favor of the fork. Do not retain an upstream rewrite merely because it is in the same file as a valid fix.
3. Existing fork behavior is the baseline. An upstream change to Codex backend calls, protocol translation, scheduling, provider wiring, configuration, management APIs, or storage may be integrated only when it preserves all currently working fork semantics and custom endpoints. If that cannot be proved, keep the fork behavior and ask the user rather than making a judgment call.
4. Never wholesale accept an upstream version of a fork-owned function or file. Integrate the smallest necessary upstream behavior; preserve custom branches, registrations, filters, routes, call sites, configuration fields, and error handling.
5. Before committing, run `gofmt`, the regression pins below, relevant focused tests for every changed behavior, `go test ./...`, and `go build -o test-output ./cmd/server && rm test-output`. For paths without automated coverage, trace the relevant end-to-end call path manually. A clean build is not sufficient proof.

Mechanical release gate:
- Explicitly read this file and `../AGENTS.md` before fetching or merging.
- Record `PRE=$(git rev-parse HEAD)` and inspect the uncommitted result with `git diff --cached "$PRE"`. Never use `git diff "$PRE" HEAD` for this purpose because `HEAD` is still the pre-merge commit.
- After resolving or combining changes, inspect `git status --short`, `git diff`, and `git diff --cached`. Stage the intended final result, then rerun `git diff --cached "$PRE"`; never review or commit the initial staged merge while fork corrections remain unstaged.
- Maintain a complete path/function ledger: **category bucket (1/2/3)**, upstream behavior, fork ownership, retain/exclude/combine decision, and proof/test. No changed path or function may remain unclassified. Bucket-3 rows must be exclude.
- Before merging, identify files changed on both sides from the merge base; all are mandatory manual review even without conflicts.
- More than 20 changed files, more than 500 changed lines, or any protected subsystem requires a second read-only staged-index audit before commit.
- Do not pipe `go test` or `go build` through output filters. Release checks must preserve the actual command exit code.
- Presence greps, ancestry, no deleted files, “0 behind,” compile success, and existing tests alone do not prove preservation.
- Before commit, report exact changed-file/line counts and retain/exclude/combine counts. Any unproven behavior must use the fork implementation or be escalated to the user.

If the gate finds a removed, changed, or unproven fork behavior, restore/combine the fork implementation before committing, or abort the merge. Never commit an unsafe merge expecting a later revert to repair it.

**Upstream may replace fork code only when** it is bucket-1 protocol tracking, fixes the same broken path, implements the same feature more robustly **without** dropping fork semantics or regressing a superior fork feature, or is security/deploy-blocking in that area. Upstream must never replace a superior fork feature “because upstream differs.”

**Stop and ask the user first** before accepting upstream that:
- is a bucket-2 feature that looks useless, speculative, or conflicts with fork custom work
- would replace a fork feature the fork already implements as well or better
- refactors or rewrites fork-owned product behaviour without a clear bug fix / protocol need
- would drop or break any item under “Fork features to preserve”
- is large/ambiguous and you cannot prove fork features still work

**Never merge bucket-3 sponsor/promo content** (see category table). Do not treat “clean merge” or “small diff” as permission to keep sponsor code.

On conflicts: apply the category tie-breaker. For bucket 2, **keep fork features** by default; take upstream only for the narrow cases above; combine when both matter. Never resolve by taking an entire upstream file/function, deleting fork-only files, or silently dropping selection filters / routes.

- **File splits are high-risk.** Upstream regularly splits large files (`service.go`, `conductor.go`, `config.go`, `server.go`, `auth_files.go`, provider executors) into `*_topic.go`. A clean compile does **not** mean fork logic survived — switch cases, route tables, and call sites can disappear silently.
- After any upstream merge that touches those areas, diff pre-merge vs HEAD for:
  - management routes (`mgmt.GET/PUT/...`)
  - provider switch cases in `registerExecutorForAuth` / `registerModelsForAuth*`
  - `baselineExecutorAuths` provider list
  - scheduler selection filters (private-instructions, free-auth, etc.)
  - `usagestore.Configure` call sites
  - auth-file metadata sync hooks (`syncAuthFileMetadataFields`)

### Dedicated fork files (prefer these so future splits cannot drop them)
| File | Purpose |
|---|---|
| `internal/config/fork_failure_policy.go` | xAI/Codex failure policy + Codex private-instructions config types |
| `sdk/cliproxy/auth/conductor_fork_failure_policy.go` | auto-disable, private-instructions policy helpers, exhaustion counters |
| `sdk/api/handlers/handlers_private_instructions.go` | request private-instructions marker injection |
| `internal/runtime/executor/codex_executor_fork_instructions.go` | apply configured Codex instructions |
| `internal/api/handlers/management/auth_files_fork_enrichment.go` | Codex plan / private-instructions / xAI status enrichment |
| `internal/api/handlers/management/auth_files_qoder_oauth.go` | Qoder + Qoder CN device-login management handlers |

### Fork features to preserve (do not silently drop)
- **Qoder CN (`qodercn`) and Qoder international (`qoder`)** device-login providers:
  - executor registration (`NewQoderCNExecutor` / `NewQoderExecutor`) in `sdk/cliproxy/service_executors.go`
  - model registration (`FetchQoderCNModels` / `FetchQoderIntlModels`) in `sdk/cliproxy/service_models.go`
  - `baselineExecutorAuths` must list both providers
  - auth load paths: `sdk/auth/filestore.go`, `internal/watcher/synthesizer/file.go`
  - management routes `/qodercn-auth-url`, `/qoder-auth-url`
  - CLI flags `--qodercn-login`, `--qoder-login`
- **Codex private instructions**: policy must run in **all** auth selection paths (`pickViaBuiltinScheduler`, `pickNext`, `pickNextLegacy`, `pickNextMixed`, `pickNextMixedLegacy`). Helpers live in `conductor_fork_failure_policy.go`. PATCH auth-file must call `syncAuthFileAllowPrivateInstructionsAttribute`.
- xAI auto-disable after surviving 401 (and permission-denied 403) when config enabled
- Codex auto-disable / exhaustion handling (`trackCodexExhaustionCounter` / `disableCodexAuth`)
- Usage monitoring: `usage-store-path` / `usage-retention-days`, `usagestore.Configure` on startup **and** reload, routes `/usage-events`, `/usage-summary`, `/usage-filter-options`, `/usage-account-stats`, `/usage-api-key-stats`, model-prices family
- Distinct auth scheduler behavior (`auth_unavailable` when candidates exist)
- Model context overrides (`model-context-overrides` management API + registry apply path)
- Single data root (`CLIPROXY_DATA_DIR`, default `/data`): config/auths/logs/plugins/usage.db under one mount
- Fork-owned management UI acquisition: `DefaultPanelGitHubRepository`, updater release/fallback URLs, stale upstream-default migration, and the cached/served `management.html` must resolve to `josephcy95/Cli-Proxy-API-Management-Center`
- Primary Chinese README / fork README choices
- **No sponsor/promo content** — never reintroduce ads, sponsor badges, donate chrome, forced attribution, or marketing splash from upstream

### Retired route (excluded by design)
- The dedicated ChatGPT Excel/Basispoints model route was removed at the user's request. Do not restore `internal/excel`, Excel executor dispatch, catalog aliases, eligibility rules, or runtime config types. YAML migration discards legacy `excel-api-key` and canonical `api-keys.excel`; ordinary Codex OAuth and generic spreadsheet tools/MIME support remain.

### Regression pins (keep these green)
- `TestManagementRoutesAreRegistered` — management routes the UI depends on
- `TestRegisterAvailableExecutors` — must include `qodercn` and `qoder`
- Private-instructions policy unit tests under `sdk/cliproxy/auth/`

### Ship policy
See workspace parent `../AGENTS.md` if present. In short:
- **Minor** → no push/tag/release unless asked (commit only if needed).
- **Medium** → commit when done; offer push/release.
- **Meaningful** (upstream merge, user-facing/deploy-blocking, multi-file feature) → push + tag + release + docker tags.
- Version tags: next fork `v7.2.x` (may exceed upstream numbers on origin only).

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Data root: `CLIPROXY_DATA_DIR` / `CLI_PROXY_DATA_DIR` (default `/data`)
- Default config: `$DATA/config.yaml` (template: `config.example.yaml`; Docker entrypoint seeds if missing)
- `.env` loaded from data root first, then working directory
- Auth / logs / plugins / usage: `$DATA/auths`, `$DATA/logs`, `$DATA/plugins`, `$DATA/usage.db`
- Docker: single volume host→`/data` (do not mount over `/CLIProxyAPI`)
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket, Qoder/Qoder CN)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` / `internal/usagestore/` — Usage and durable monitoring store
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline). Provider wiring lives in split files: `service_executors.go`, `service_models.go`, `service_auth.go`, …
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
