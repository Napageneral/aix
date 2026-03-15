---
name: aix
description: Use AIX for engineer token issuance, source registration, run and upload tracking, and imported AI-session review across distributed devices.
---

# AIX

Use this package when working on the AIX app package root.

AIX is a standalone Nex app that collects, receives, stores, and reviews
imported AI session history from many engineer devices. It is the ingestion
product. It is not a Spike feature.

## When To Use It

- issue or rotate engineer-bound AIX client tokens
- register or reconnect engineer devices as AIX sources
- inspect source health, runs, uploads, and imported sessions
- validate the machine-upload contract used by the `aix` CLI
- reason about the AIX data model without collapsing entity, source, and device
  into one concept

## Do Not Use It For

- reviving the old `sessions.import*` customer flow
- guessing hosted runtime URLs from browser origin
- treating device identity as the canonical owner of imported history

## Main Methods

### Human-Facing Methods

- `aix.credentials.issue`
- `aix.credentials.list`
- `aix.credentials.revoke`
- `aix.credentials.rotate`
- `aix.entities.list`
- `aix.sources.list`
- `aix.sources.get`
- `aix.sources.update`
- `aix.runs.list`
- `aix.runs.get`
- `aix.imported-sessions.list`

### Device-Facing Methods

- `aix.sources.register`
- `aix.runs.begin`
- `aix.runs.complete`
- `aix.uploads.begin`
- `aix.uploads.chunk`
- `aix.uploads.status`
- `aix.uploads.complete`

## Runtime Operation Examples

Issue one engineer-bound credential and setup bundle:

```bash
curl -X POST "$RUNTIME_BASE_URL/runtime/operations/aix.credentials.issue" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "entityId": "ent_engineer_123",
    "preset": "ongoing",
    "defaultMode": "incremental",
    "defaultCadence": "five_minutes"
  }'
```

Register one source from the machine side:

```bash
curl -X POST "$RUNTIME_BASE_URL/runtime/operations/aix.sources.register" \
  -H "Authorization: Bearer $AIX_CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "installId": "aix_install_123",
    "clientDeviceId": "device_laptop_123",
    "clientVersion": "0.1.0",
    "providers": [
      { "provider": "codex", "enabled": true }
    ]
  }'
```

List imported sessions for one entity:

```bash
curl -X POST "$RUNTIME_BASE_URL/runtime/operations/aix.imported-sessions.list" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "entityId": "ent_engineer_123",
    "limit": 20
  }'
```

## CLI Examples

The normal engineer flow is through the `aix` CLI:

```bash
aix init
aix connect --runtime "$RUNTIME_BASE_URL" --token "$AIX_CLIENT_TOKEN"
aix push --mode backfill
```

For recurring sync:

```bash
aix daemon enable --source codex --cadence five_minutes
```

## Package-Internal Nex SDK Examples

Inside the AIX app, runtime-owned behavior should go through `ctx.nex`.

Issue a runtime auth token:

```ts
const created = await ctx.nex.auth.tokens.create({
  label: "AIX device token",
  subject_id: entityId,
});
```

Create or reuse the shared archive workspace:

```ts
const listed = await ctx.nex.workspaces.list({
  kind: "app",
  limit: 10,
});
```

Read imported-session provenance through the canonical runtime API:

```ts
const imports = await ctx.nex.agents.sessions.imports.list({
  workspace_id,
  limit: 20,
});
```

## Key Data Models

- engineer entity
  - canonical owner of imported session history
- AIX credential
  - revocable runtime auth token issued for one engineer entity
- AIX source
  - one long-lived local AIX installation, keyed by `install_id`
- device
  - operational machine identity, useful for troubleshooting but not canonical
  - owner identity
- AIX run
  - one external sync + upload execution
- AIX upload
  - one resumable remote payload upload under a run
- imported session
  - canonical imported session rows written into `agents.db`
- `AIX Archive` workspace
  - shared workspace used for imported-session readback

Important distinctions:

- one entity may own many sources
- reconnect with the same `install_id` should reuse the same source
- device metadata is not the canonical owner key

## End-To-End Example

Normal operator and engineer flow:

1. operator calls `aix.entities.list`
2. operator calls `aix.credentials.issue`
3. engineer runs:
   - `aix init`
   - `aix connect`
   - `aix push --mode backfill`
4. machine side calls:
   - `aix.sources.register`
   - `aix.runs.begin`
   - `aix.uploads.*`
   - `aix.runs.complete`
5. operator verifies the result with:
   - `aix.sources.list`
   - `aix.runs.list`
   - `aix.imported-sessions.list`

This matches the main AIX validation ladder: credential lifecycle -> source
registration -> run/upload lifecycle -> imported-session finalize -> CLI
connect and push.

## Constraints And Failure Modes

- In hosted mode, the setup bundle must use the server's
  `runtime_public_base_url`.
- The setup bundle must not use:
  - `window.location.origin`
  - loopback runtime URLs in hosted mode
  - frontdoor browser `/runtime` proxy URLs
- AIX app state lives in app storage; imported session corpus lives in
  `agents.db`.
- Device-side operations are normal runtime operations; they are not a separate
  private transport contract.

## Related Docs

- `app.nexus.json`
- `docs/specs/AIX_APP.md`
- `docs/specs/AIX_APP_API_AND_CLI.md`
- `docs/validation/AIX_APP_VALIDATION_LADDER.md`
