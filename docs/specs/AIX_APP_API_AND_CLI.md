# AIX App API And CLI

**Status:** CANONICAL
**Last Updated:** 2026-03-11

---

## Purpose

This document locks the AIX app contract for:

1. credential issuance
2. source registration
3. run tracking
4. resumable upload transport
5. imported-session readback
6. AIX CLI behavior

Related active docs:

- [AIX App](/Users/tyler/nexus/home/projects/nexus/apps/aix/docs/specs/AIX_APP.md)

---

## Customer Experience

Operator flow:

1. open the AIX app
2. browse or search engineer entities inside the app
3. issue one AIX client token
4. copy the setup bundle
5. monitor sources, runs, and imported sessions

Engineer flow:

1. install `aix`
2. run `aix init`
3. run `aix connect`
4. run `aix push --mode backfill` once
5. optionally run `aix daemon enable --source codex --cadence five_minutes|ten_minutes|daily` or `aix live --remote`

---

## Setup Bundle Contract

The setup bundle returned by `aix.credentials.issue` or `aix.credentials.rotate`
must contain:

```ts
{
  runtimeBaseUrl: string; // hosted: server runtime_public_base_url
  token: string;
  commands: string[];
  promptText: string;
}
```

Rules:

1. in hosted deployments, `runtimeBaseUrl` must equal the server's `runtime_public_base_url`
2. `runtimeBaseUrl` must not be inferred from `window.location.origin`
3. `runtimeBaseUrl` must not be the frontdoor browser `/runtime` proxy URL
4. local or isolated dev may use loopback runtime URLs

Hosted resolution rule:

1. when `runtimeBaseUrl` is omitted in hosted mode, the AIX app resolves it by
   calling `productControlPlane.call`
2. the required product operation is `aix.hostedContext.get`
3. frontdoor returns the selected server's `runtime_public_base_url`
4. the AIX app uses that returned value in the setup bundle

---

## Token Model

AIX issues one client token per engineer entity.

Required properties:

- entity-bound
- revocable
- optional expiry
- reusable across that engineer's sources
- scoped to AIX device operations

Required AIX device permission set:

1. `core.apps.aix.sources.register.write`
2. `core.apps.aix.runs.begin.write`
3. `core.apps.aix.runs.complete.write`
4. `core.apps.aix.uploads.begin.write`
5. `core.apps.aix.uploads.chunk.write`
6. `core.apps.aix.uploads.status.read`
7. `core.apps.aix.uploads.complete.write`
8. `core.apps.aix.sources.get.read`
9. `core.workspaces.read`
10. `core.workspaces.write`
11. `core.agents.sessions.import.admin`
12. `core.agents.sessions.imports.read`

The current runtime SDK preserves the caller's token scope budget when inline
app handlers invoke runtime-owned operations. Until that platform contract
changes, AIX client tokens must include the transitive runtime permissions the
inline AIX app depends on.

---

## AIX App Operations

### Human-facing operations

These are used by the AIX UI.

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

### Device operations

These are used by the local AIX CLI.

- `aix.sources.register`
- `aix.runs.begin`
- `aix.runs.complete`
- `aix.uploads.begin`
- `aix.uploads.chunk`
- `aix.uploads.status`
- `aix.uploads.complete`

All are normal runtime operations.

Transport is caller choice:

- AIX UI uses the runtime API over HTTP from the browser app
- AIX CLI typically uses the runtime API over HTTP

The operation names and schemas are the same across both transports.

Rule:

- all AIX operator reads required by the browser UI must be reachable through
  the runtime API over HTTP

---

## Key Method Contracts

### `aix.credentials.issue`

Input:

```ts
{
  entityId: string;
  preset: "offboarding" | "ongoing";
  label?: string;
  expiresAt?: number | null;
  providerAllowlist?: string[];
  defaultMode?: "backfill" | "incremental" | "live";
  defaultCadence?: "manual" | "five_minutes" | "ten_minutes" | "daily" | "live";
  runtimeBaseUrl?: string;
}
```

Rule:

- when hosted, the setup bundle must ultimately use `runtime_public_base_url`
- the caller may provide `runtimeBaseUrl` explicitly, but the canonical hosted
  path is backend resolution through `aix.hostedContext.get`

Output:

```ts
{
  ok: true;
  credential: {
    id: string;
    entityId: string;
    tokenId: string;
    label: string | null;
    expiresAt: number | null;
    revokedAt: number | null;
    createdAt: number;
    lastUsedAt: number | null;
    preset: "offboarding" | "ongoing";
  };
  token: string;
  setupBundle: {
    runtimeBaseUrl: string;
    token: string;
    commands: string[];
    promptText: string;
  };
}
```

### `aix.sources.register`

Input:

```ts
{
  installId: string;
  clientDeviceId: string;
  hostname?: string;
  platform?: string;
  arch?: string;
  osVersion?: string;
  clientVersion: string;
  label?: string;
  defaultMode?: "backfill" | "incremental" | "live";
  expectedCadence?: "manual" | "five_minutes" | "ten_minutes" | "daily" | "live";
  providers: Array<{
    provider: string;
    enabled: boolean;
  }>;
}
```

Output:

```ts
{
  ok: true;
  source: {
    id: string;
    entityId: string;
    installId: string;
    currentDeviceId: string;
    status: "active" | "paused" | "revoked" | "error";
    defaultMode: "backfill" | "incremental" | "live";
    expectedCadence: "manual" | "five_minutes" | "ten_minutes" | "daily" | "live";
  };
  runtime: {
    baseUrl: string;
    operationsPath: "/runtime/operations/";
  };
}
```

Rule:

- `runtime.baseUrl` returned to the CLI must remain the machine-upload base URL

### `aix.runs.begin`

This operation is idempotent on `(sourceId, clientRunId)`.

### `aix.uploads.chunk`

This operation is idempotent on upload id, chunk index, and chunk checksum.

### `aix.uploads.complete`

Finalize rules:

1. decode the uploaded payload
2. resolve or create the shared `AIX Archive` workspace
3. invoke the imported-session bridge
4. persist counters and provenance
5. delete spool files immediately on success

---

## CLI Contract

### `aix connect`

Purpose:

- create local ids when missing
- persist remote config
- call `aix.sources.register`

Canonical form:

```bash
aix connect --url <runtimeBaseUrl> --token <aixClientToken>
```

### `aix push`

Purpose:

- run local sync
- begin remote run
- upload changed sessions
- finalize remote run

Canonical forms:

```bash
aix push --mode backfill
aix push --mode incremental
```

### `aix daemon enable`

Purpose:

- install a managed recurring scheduler for incremental pushes

Canonical form:

```bash
aix daemon enable --source codex --cadence five_minutes
```

Rules:

- macOS uses a per-user LaunchAgent
- Linux uses a per-user systemd service and timer
- `--source` is required and pins the recurring job to one source scope
- the scheduled command is `aix push --source <source> --mode incremental`
- enable must fail until `aix connect` has persisted remote config
- supported cadences are:
  - `five_minutes`
  - `ten_minutes`
  - `daily`

### `aix daemon disable`

Purpose:

- unload and remove the managed scheduler

### `aix live --remote`

Purpose:

- reuse the local watcher path and remote upload path together

---

## Imported Session Read Model

`aix.imported-sessions.list` is the AIX read facade over imported data in
`agents.db`.

Rules:

1. reads are scoped to the AIX app's shared `AIX Archive` workspace
2. results may be filtered by entity, source, provider, and cursor
3. results are operationally useful for monitoring and review, not a replacement for direct `agents.db` access
4. the inline AIX app must fulfill this facade through the runtime-owned
   `agents.sessions.imports.list` operation, not direct ledger access from the packaged app
