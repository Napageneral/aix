# AIX Hosted Product Workplan

**Status:** ACTIVE
**Created:** 2026-03-11

---

## Purpose

This workplan closes the gap between the current AIX implementation and the
canonical AIX product described in:

- [AIX App](/Users/tyler/nexus/home/projects/nexus/apps/aix/docs/specs/AIX_APP.md)
- [AIX App API And CLI](/Users/tyler/nexus/home/projects/nexus/apps/aix/docs/specs/AIX_APP_API_AND_CLI.md)

---

## Customer Goal

The target hosted customer flow is:

1. publish a Nex server
2. install the AIX app through the normal hosted app install flow
3. issue an AIX client token for an engineer entity
4. hand the engineer a setup bundle with the server's `runtime_public_base_url`
5. run local `aix connect`
6. run local `aix push --mode backfill`
7. watch the resulting source, run, and imported sessions in the AIX app
8. optionally enable recurring sync with `aix daemon enable --source <src> --cadence five_minutes|ten_minutes|daily`

---

## Current State

Completed foundation:

1. AIX exists as a standalone app package at `/Users/tyler/nexus/home/projects/nexus/apps/aix/app`
2. AIX app methods exist for credentials, sources, runs, uploads, and imported-session reads
3. AIX finalizes imports into `agents.db`
4. the app uses one shared `AIX Archive` workspace per install
5. the AIX CLI supports:
   - `aix connect`
   - `aix push`
   - `aix live --remote`
   - `aix daemon enable`
   - `aix daemon disable`
6. an isolated live-stack e2e already proves:
   - frontdoor login
   - runtime token mint
   - runtime app install
   - AIX token issue
   - real `aix connect`
   - real `aix push`
   - provenance in `agents.db`

Remaining hosted-product gaps:

1. active AIX docs have been living outside the app-local canonical docs tree
2. the AIX manifest did not yet advertise product metadata for frontdoor catalog sync
3. hosted setup bundles still depend on the caller providing the correct `runtimeBaseUrl`
4. frontdoor product/catalog/install wiring for AIX is not yet proven as a normal product path
5. the customer-facing AIX UI is still skeletal
6. current Nex code still contains explicit transport-surface gating even though the canonical Nex model treats surfaces as transport, not API
7. the real published-server drill still depends on a published AIX package release variant being present in the frontdoor registry and artifact storage

Observed live production blockers from the March 11 drill:

1. production frontdoor does not currently publish `aix` in `/api/apps/catalog`,
   so the normal customer install route fails before install planning starts
2. the disposable-account drill path did not produce a fresh isolated server,
   so a clean signup-based production drill is not currently reliable
3. public runtime URL semantics are inconsistent across live code and live
   APIs:
   - `getServerPublicUrl(server)` returns `https://<tenantId>.nexushub.sh`
   - `resolveRuntimeDescriptor()` currently builds `<frontdoor-origin>/runtime`
   - live server APIs have also returned `srv-...nexushub.sh`
4. public tenant-origin routing is not yet serving the runtime HTTP contract on
   the advertised machine-upload origins, so AIX cannot use the real customer
   upload path against production even though the private runtime is healthy
5. manual private staging is a valid hosted lifecycle fallback because
   frontdoor-owned staging over the private network is canonical, but manually
   invoking it is still only a temporary validation path, not the customer flow

Observed live production result from the March 12 drill:

1. the normal hosted install and upgrade path is now working for `aix`
2. the real published-server drill succeeded on:
   - server `srv-5a40b00b-e00`
   - tenant `t-4ef660eb-007`
   - public runtime origin `https://t-4ef660eb-007.nexushub.sh`
3. the real local AIX CLI path succeeded:
   - `aix connect`
   - `aix push --source codex --mode backfill`
4. the live run completed with:
   - `3540` sessions seen
   - `3540` sessions changed
   - `3539` imported
   - `1` upserted
   - `0` failed
5. the final production-only blocker was a Nex core schema-cutover bug:
   - live `agents.db.turns` was missing `working_dir`
   - AIX imports were failing with `table turns has no column named working_dir`
   - the proper fix was adding `ensureTurnsWorkingDirColumn()` to `ensureAgentsSchema()`
6. a remaining non-blocking follow-up is that raw tenant-origin HTTP control
   readback for `aix.sources.list`, `aix.runs.list`, and
   `aix.imported-sessions.list` is still not part of the published-server drill
   proof path; the production drill was verified directly through the live AIX
   control DB and `agents.db`
7. the hosted browser/operator drill is now also passing on the same server:
   - frontdoor shell launch works
   - embedded AIX app iframe works
   - live `Sources`, `Runs`, and `Imported Sessions` render
   - `aix.credentials.issue` returns a live setup bundle with the tenant public
     runtime origin
8. the required hosted runtime auth shape is now explicit:
   - public tenant-origin ingress uses `trusted_token`
   - runtime-initiated product-runtime API calls still require the server
     private `NEXUS_RUNTIME_TOKEN`

---

## Phase 1: Canonical AIX Docs Rebase

**Goal:** The active AIX docs live under `apps/aix/docs/` and tell one coherent story.

Tasks:

1. create app-local `specs/`, `workplans/`, and `validation/` directories
2. rewrite the AIX specs to match the locked Nex canon
3. remove stale assumptions from the AIX specs:
   - hybrid inline-plus-service app model
   - `window.location.origin` setup bundle rule
   - frontdoor browser proxy as machine-upload path
4. point future implementation and validation work at the app-local docs

---

## Phase 2: Product Catalog Readiness

**Goal:** AIX can appear as a hosted product in frontdoor's product catalog.

Tasks:

1. add product metadata to the AIX app manifest
2. validate that frontdoor product sync can ingest the AIX manifest
3. ensure deployment packaging exposes the AIX manifest to `FRONTDOOR_PRODUCT_MANIFEST_PATHS`
4. prove `store.getProduct("aix")` succeeds in the hosted install path
5. add a first-class app publish flow that writes both:
   - product catalog state for `aix`
   - package registry release + variant state for `aix`
6. ensure the published AIX package remains package-safe and only reaches core ledgers through runtime-owned operations

Primary areas:

- `/Users/tyler/nexus/home/projects/nexus/apps/aix/app/app.nexus.json`
- `/Users/tyler/nexus/home/projects/nexus/nexus-frontdoor/src/product-sync.ts`
- `/Users/tyler/nexus/home/projects/nexus/nexus-frontdoor/src/server.ts`

---

## Phase 3: Hosted Setup Bundle Runtime URL

**Goal:** Hosted setup bundles always contain the correct machine-upload base URL.

Tasks:

1. define the canonical source of `runtime_public_base_url` for AIX setup flows
2. implement `aix.hostedContext.get` as a frontdoor-fulfilled
   `productControlPlane.call` operation
3. make `aix.credentials.issue` and `aix.credentials.rotate` resolve hosted
   setup URLs through that operation when no explicit override is supplied
4. remove operator dependence on loopback or browser-origin guessing in hosted flows
5. ensure `aix.sources.register` and returned runtime metadata stay aligned with that machine-upload URL

Primary decision:

- the runtime origin used by the AIX CLI is the server `runtime_public_base_url`
- the AIX app backend, not the browser UI, is authoritative for hosted setup-bundle URL resolution

Not allowed:

- `window.location.origin`
- `http://127.0.0.1:<port>` in hosted setup bundles
- frontdoor `/runtime` browser proxy URLs

---

## Phase 4: Hosted Install Path

**Goal:** AIX installs through the normal frontdoor-managed app install path.

Tasks:

1. prove `POST /api/servers/:id/apps/aix/install` succeeds on a hosted server
2. ensure the installed app is visible in the tenant app catalog and nav
3. verify the AIX app can be opened after install with normal hosted routing
4. ensure production frontdoor startup includes the AIX manifest in
   `FRONTDOOR_PRODUCT_MANIFEST_PATHS`
5. prove the frontdoor registry contains a published AIX package release
   variant and tarball
6. hard-cut app install planning to resolve the latest published release
   instead of looking up a literal version string `"latest"`
7. record any package registry or product-catalog prerequisites explicitly
8. remove manual private staging from the primary install path for AIX product
   validation

Required outcomes:

1. `/api/apps/catalog` contains `aix`
2. `store.getProduct("aix")` succeeds in the hosted install path
3. the published release artifact exists and is selectable by the install
   planner

---

## Phase 5: AIX Customer UI

**Goal:** The AIX app is operable without ad hoc runtime calls.

Tasks:

1. expose operator-safe AIX read methods on tenant-origin runtime API HTTP:
   - `aix.entities.list`
   - `aix.sources.list`
   - `aix.runs.list`
   - `aix.imported-sessions.list`
2. build `Setup` for entity selection and token issuance
3. build `Sources`
4. build `Runs`
5. build `Imported Sessions`
6. show health, last success, last error, and cadence
7. surface the exact setup bundle and prompt text the operator needs
8. validate the browser UI inside the hosted frontdoor shell without DB access

---

## Phase 6: Production Drill

**Goal:** Prove the real hosted operator flow on a published server.

Tasks:

1. use an existing frontdoor operator account or create a dedicated drill account through the normal frontdoor signup flow
2. provision or select a real hosted server
3. verify the account has free-tier or paid capacity to create the server
4. if the drill uses a fresh signup path instead of an existing operator
   account, verify that signup does not attach the account to a shared existing
   tenant
5. verify the selected server exposes one canonical tenant-origin runtime base
   URL that serves `/runtime/...` for runtime access tokens
6. verify `POST /api/runtime/token` returns that same tenant-origin runtime
   descriptor instead of a frontdoor-shell proxy descriptor
7. install AIX through the hosted app install path
8. issue a real AIX client token
9. run local `aix connect`
10. run local `aix push --mode backfill`
11. verify:
   - source registration
   - run completion
   - imported sessions in `AIX Archive`
   - provenance rows in `agents.db`
12. run one recurring sync drill with `aix daemon enable --source codex --cadence five_minutes`
13. record the exact live drill in the validation runbook

This phase is not satisfied by a shared tenant, a manual private install path,
or a frontdoor-shell-only runtime proxy. It must exercise the same routing and
install mechanics a real customer receives.

Interim validation note:

- if production frontdoor still does not publish `aix`, run the hosted
  manual-install drill on a frontdoor-provisioned server to validate the real
  hosted runtime path without treating the hosted install-path gap as solved

---

## Phase 7: Nex Canon Cleanup

**Goal:** Remove AIX-local dependencies on stale Nex transport assumptions once the core cutover lands.

Tasks:

1. stop depending on app-method surface declarations as part of the AIX product contract
2. align AIX docs and implementation to the final Nex surface model
3. archive the stale root-level AIX planning docs once the app-local set is the only active truth

---

## Exit Criteria

This workplan is complete when:

1. AIX is cataloged as a hosted product
2. AIX installs through the normal hosted install path
3. issued setup bundles contain the correct `runtime_public_base_url`
4. a real engineer laptop can run `aix connect` and `aix push` against a published server
5. the AIX app UI can show sources, runs, and imported sessions for that flow
6. recurring sync is proven on the same hosted path
7. the published-server drill is reproducible from the active validation runbook
