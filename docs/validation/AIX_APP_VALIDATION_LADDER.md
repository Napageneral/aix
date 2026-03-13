# AIX App Validation Ladder

**Status:** ACTIVE
**Last Updated:** 2026-03-12

---

## Purpose

This ladder proves the AIX app behaves according to the active AIX specs.

Validation must climb in order.

---

## Rung 1. Manifest and package validity

Validate:

1. the AIX app manifest parses and validates
2. the inline handler loads correctly
3. hooks resolve inside the app package root
4. the packaged handler does not depend on repo-relative Nex runtime or ledger modules at load time

Primary anchors:

- `/Users/tyler/nexus/home/projects/nexus/apps/aix/app/app.nexus.json`
- `/Users/tyler/nexus/home/projects/nexus/nex/src/apps/aix-package.test.ts`

---

## Rung 2. Product metadata sync

Validate:

1. frontdoor product sync can ingest the AIX app manifest
2. a product record for `aix` is created in the frontdoor store
3. the synced metadata matches the AIX manifest product section

Primary anchors:

- `/Users/tyler/nexus/home/projects/nexus/nexus-frontdoor/src/product-sync.ts`
- AIX manifest product-sync test

---

## Rung 3. Credential lifecycle

Validate:

1. `aix.credentials.issue` creates a runtime auth token and mirrored AIX credential row
2. `aix.credentials.list` returns issued credentials
3. `aix.credentials.revoke` revokes the runtime auth token
4. `aix.credentials.rotate` returns a new token and setup bundle

Primary anchors:

- `/Users/tyler/nexus/home/projects/nexus/apps/aix/app/methods/index.ts`
- `/Users/tyler/nexus/home/projects/nexus/nex/src/apps/aix-package.test.ts`

---

## Rung 4. Source registration

Validate:

1. first `aix.sources.register` creates device and source rows
2. reconnect with the same `install_id` reuses the same source
3. one entity can own multiple sources
4. the same `install_id` under a different entity is rejected

---

## Rung 5. Run and upload lifecycle

Validate:

1. `aix.runs.begin` is idempotent on `(sourceId, clientRunId)`
2. `aix.uploads.begin`, `chunk`, `status`, and `complete` support resume
3. successful finalize removes spool files
4. failed uploads remain inspectable until cleanup

---

## Rung 6. Imported-session finalize path

Validate:

1. finalized uploads write into `agents.db`
2. imported rows preserve `source_entity_id`, `aix_source_id`, and `last_run_id`
3. the app creates exactly one shared `AIX Archive` workspace per install
4. imported-session reads stay scoped to that workspace

Primary anchors:

- `/Users/tyler/nexus/home/projects/nexus/nex/src/nex/import/service.test.ts`
- `/Users/tyler/nexus/home/projects/nexus/nex/src/nex/import/validation.test.ts`

---

## Rung 7. CLI connect and push

Validate:

1. `aix connect` persists the operator-supplied runtime URL without rewriting it
2. `aix push --mode backfill` performs local sync before remote upload
3. zero-change pushes complete without upload
4. changed sessions upload and finalize cleanly

Primary anchors:

- `/Users/tyler/nexus/home/projects/aix/cmd/aix/remote_test.go`
- `/Users/tyler/nexus/home/projects/aix/cmd/aix/remote.go`

---

## Rung 8. Managed scheduler

Validate:

1. `aix daemon enable --source <src> --cadence five_minutes|ten_minutes|daily` writes the expected LaunchAgent or systemd units
2. the scheduled command is `aix push --source <src> --mode incremental`
3. `aix daemon disable` unloads and removes the managed scheduler
4. `aix live --remote` reuses the local watcher and remote upload path

Primary anchors:

- `/Users/tyler/nexus/home/projects/aix/cmd/aix/daemon.go`
- `/Users/tyler/nexus/home/projects/aix/cmd/aix/daemon_test.go`

---

## Rung 9. Isolated hosted live-stack e2e

Validate:

1. frontdoor login succeeds
2. runtime token mint succeeds
3. AIX app install succeeds on the runtime
4. a real AIX CLI binary can connect and push
5. the resulting source, run, archive workspace, and imported rows are correct

Primary anchors:

- `/Users/tyler/nexus/home/projects/nexus/nex/src/nex/runtime-api/server.frontdoor-live-stack.e2e.test.ts`
- `/Users/tyler/nexus/home/projects/nexus/nex/src/nex/runtime-api/server.aix-cli-live-stack.e2e.test.ts`

---

## Rung 10. Hosted install path

Validate:

1. frontdoor catalog contains product `aix`
2. frontdoor registry contains a published AIX package release variant and tarball
3. the app publish flow writes both product-catalog and package-registry state
4. `POST /api/servers/:id/apps/aix/install` resolves the latest published release by default
5. `POST /api/servers/:id/apps/aix/install` succeeds on a hosted server
6. the installed app appears in the tenant app catalog and can be opened

This rung is blocked until production frontdoor actually publishes `aix` and
the release artifact is available to the hosted install planner.

---

## Rung 11. Hosted context resolution

Validate:

1. `productControlPlane.call` with `aix.hostedContext.get` succeeds for an
   installed AIX app
2. frontdoor fulfills the request directly from selected-server hosted context
3. the returned payload includes `server_id`, `tenant_id`, and
   `runtime_public_base_url`
4. no external product control plane route is required for this operation

---

## Rung 12. Hosted setup bundle correctness

Validate:

1. the setup bundle produced in hosted mode contains `runtime_public_base_url`
2. the setup bundle never contains loopback runtime URLs in hosted mode
3. the setup bundle never contains frontdoor `/runtime` browser proxy URLs

---

## Rung 13. Hosted operator HTTP readback

Validate:

1. `aix.entities.list` succeeds on tenant-origin `POST /runtime/operations/aix.entities.list`
2. `aix.sources.list` succeeds on tenant-origin `POST /runtime/operations/aix.sources.list`
3. `aix.runs.list` succeeds on tenant-origin `POST /runtime/operations/aix.runs.list`
4. `aix.imported-sessions.list` succeeds on tenant-origin `POST /runtime/operations/aix.imported-sessions.list`
5. the responses are sufficient for the operator UI without DB access

---

## Rung 14. Hosted browser operator UI

Validate:

1. the AIX app opens inside the frontdoor shell
2. the Setup view lists engineer entities
3. issuing a token renders the setup bundle and prompt text
4. Sources, Runs, and Imported Sessions load through browser calls to
   `/runtime/operations/...`
5. the UI can monitor the live hosted drill without shell access

---

## Rung 15. Real hosted manual-install drill

Validate:

1. use an existing frontdoor operator account or a dedicated drill account created through normal frontdoor signup
2. provision or select a real hosted server through frontdoor
3. manually stage and install the AIX app onto that server through the runtime operator path
4. issue a real AIX client token
5. run local `aix connect`
6. run local `aix push --mode backfill`
7. verify the resulting source, run, and imported sessions in the AIX app and `agents.db`
8. run one recurring sync drill with `aix daemon enable --source codex --cadence five_minutes`
9. record the exact execution details in the hosted manual-install drill runbook

This rung validates the real hosted runtime path even when production frontdoor
package publishing is not yet complete. It does not satisfy the normal hosted
install-path requirement on its own.

The private staging step used here matches the canonical hosted lifecycle
ownership model, where frontdoor stages artifacts onto the server and then
calls the private runtime operator API. What makes this rung non-canonical is
the manual invocation of that staging/install path instead of the normal
customer install flow.

---

## Rung 16. Real published-server drill

Validate:

1. use an existing frontdoor operator account or a dedicated drill account created through normal frontdoor signup
2. provision or select a real published server
3. if the drill uses a fresh signup path instead of an existing operator account, verify the account is not attached to a shared existing tenant
4. verify the server exposes one canonical tenant-origin runtime base URL for machine transport
5. verify `POST /api/runtime/token` returns that same tenant-origin runtime descriptor
6. install AIX on that server through the normal hosted install path
7. issue a real AIX client token
8. run local `aix connect`
9. run local `aix push --mode backfill`
10. verify the resulting source, run, and imported sessions in the AIX app and `agents.db`
11. run one recurring sync drill with `aix daemon enable --source codex --cadence five_minutes`
12. record the exact execution details in the published-server drill runbook

March 12, 2026 result:

1. this rung passed on:
   - server `srv-5a40b00b-e00`
   - tenant `t-4ef660eb-007`
   - runtime origin `https://t-4ef660eb-007.nexushub.sh`
2. the real local AIX CLI connected and pushed successfully from the developer
   machine to the hosted server
3. the live run completed with:
   - `3540` sessions seen
   - `3540` sessions changed
   - `3539` imported
   - `1` upserted
   - `0` failed
4. proof was verified directly in:
   - `/opt/nex/data/app/aix/aix-control.db`
   - `/opt/nex/state/data/agents.db`
5. the production-only bug uncovered during this rung was a missing
   `turns.working_dir` schema cutover in Nex core; the fix is now part of
   `ensureAgentsSchema()`
6. the hosted browser/operator drill also passed after restoring the dual-path
   hosted auth contract on the tenant runtime:
   - incoming public traffic uses `trusted_token`
   - runtime-initiated `productControlPlane.call` still uses the server private
     `NEXUS_RUNTIME_TOKEN`
7. the live browser operator flow produced a real setup bundle with:
   - `runtimeBaseUrl = https://t-4ef660eb-007.nexushub.sh`
   - non-empty issued token
   - non-empty prompt text
8. the live browser operator flow also rendered hosted read surfaces for:
   - `Sources`
   - `Runs`
   - `Imported Sessions`
6. raw tenant-origin HTTP control readback for `aix.sources.list`,
   `aix.runs.list`, and `aix.imported-sessions.list` was not used as the proof
   path for this rung

---

## Rung 17. Cutover verification

Validate:

1. the app-local AIX docs are the active source of truth
2. the old root-level AIX drafts are not treated as canonical
3. the old public `sessions.import*` AIX flow is not the customer contract
