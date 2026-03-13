# AIX Hosted Manual-Install Drill

**Status:** ACTIVE
**Last Updated:** 2026-03-11

---

## Purpose

This runbook proves the hosted AIX runtime path on a real frontdoor-provisioned
server even when production frontdoor package publishing is not yet complete.

It is the concrete execution companion for rung 13 of:

- [AIX App Validation Ladder](/Users/tyler/nexus/home/projects/nexus/apps/aix/docs/validation/AIX_APP_VALIDATION_LADDER.md)

---

## Customer Experience Boundary

This drill validates:

1. real frontdoor auth
2. real frontdoor server provisioning
3. real tenant runtime access
4. real AIX token issuance
5. real local `aix connect`
6. real local `aix push --mode backfill`

This drill does **not** validate:

1. frontdoor catalog discoverability of `aix`
2. frontdoor entitlement purchase flow for `aix`
3. `POST /api/servers/:id/apps/aix/install` on production frontdoor

Those remain covered by the published-server drill and are still required for
full hosted product readiness.

---

## Preconditions

The drill requires all of the following:

1. frontdoor production origin is reachable
2. frontdoor cloud provisioning is configured
3. the acting account has free-tier or paid capacity to create a server
4. the local machine can build the AIX app tarball
5. the local machine can build or run the `aix` CLI
6. the new server is reachable over SSH using the configured hosted operator key

---

## Required Evidence

Successful execution must produce evidence for all of the following:

1. frontdoor auth succeeded
2. server provisioning succeeded
3. manual AIX app staging and runtime operator install succeeded
4. `aix.credentials.issue` returned a setup bundle with the server
   `runtime_public_base_url` using an explicit `runtimeBaseUrl` override
5. `aix connect` succeeded without rewriting the runtime URL
6. `aix push --mode backfill` completed
7. `aix.sources.list` shows the registered source
8. `aix.runs.list` shows the completed run
9. `aix.imported-sessions.list` shows imported sessions in `AIX Archive`
10. recurring sync can be enabled with `aix daemon enable --source codex --cadence five_minutes`

---

## Execution Sequence

1. authenticate to frontdoor
2. create or select the target server
3. wait until the server status is `running`
4. package the AIX app tarball locally
5. SSH/SCP the tarball to the tenant server staging directory
6. call runtime `POST /api/operator/packages/install`
7. mint a runtime token for the selected server
8. call `aix.credentials.issue` with explicit `runtimeBaseUrl =
   <server.runtime_public_base_url>`
9. run local `aix connect --url <runtime_public_base_url> --token <aix_client_token>`
10. run local `aix push --mode backfill`
11. verify sources, runs, and imported sessions through runtime operations
12. enable recurring sync locally

Reference automation:

- `/Users/tyler/nexus/home/projects/nexus/apps/aix/scripts/hosted-manual-install-drill.mjs`

---

## Notes

1. Human control still uses frontdoor.
2. Machine upload still uses the tenant runtime public base URL from the setup
   bundle.
3. Host-level access is part of this drill by design.
4. This drill is an interim hosted validation path, not a replacement for the
   normal published-server install path.
5. Because frontdoor does not own the install in this path, hosted setup-bundle
   resolution must use the explicit `runtimeBaseUrl` override rather than
   `aix.hostedContext.get`.
