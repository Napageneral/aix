# AIX Hosted Published-Server Drill

**Status:** ACTIVE
**Last Updated:** 2026-03-11

---

## Purpose

This runbook proves the real hosted AIX customer flow on a published frontdoor
server.

It is the concrete execution companion for rung 13 of:

- [AIX App Validation Ladder](/Users/tyler/nexus/home/projects/nexus/apps/aix/docs/validation/AIX_APP_VALIDATION_LADDER.md)

---

## Customer Experience

The drill must match what a real customer would do:

1. authenticate to frontdoor
2. provision or select a server
3. install the `AIX` app
4. issue an engineer token from AIX
5. run local `aix connect`
6. run local `aix push --mode backfill`
7. verify uploaded sessions appear in the AIX app

The drill is not complete if it relies on loopback runtime URLs, direct local
DB mutation, or manual setup-bundle editing.

---

## Preconditions

The drill requires all of the following:

1. frontdoor production origin is reachable
2. frontdoor cloud provisioning is configured
3. the acting account has free-tier or paid capacity to create a server
4. the frontdoor product catalog contains `aix`
5. the frontdoor package registry contains a published AIX app release variant
6. the AIX tarball for that release exists on the frontdoor artifact store
7. the local machine can build or run the `aix` CLI

If `POST /api/servers/:id/apps/aix/install` fails with `package_not_found`, the
drill must stop and the missing published release must be treated as the
blocking gap.

---

## Supported Account Paths

### Path A: existing operator account

Use an existing frontdoor account when the username or session is already known.

### Path B: dedicated drill account

Use a fresh account created through the normal frontdoor signup flow when the
existing operator login cannot be resolved locally.

This still validates the real customer experience because it uses the public
frontdoor auth, provisioning, install, and runtime access flows without
operator-side host intervention.

---

## Required Evidence

Successful execution must produce evidence for all of the following:

1. frontdoor auth succeeded
2. server provisioning succeeded
3. AIX install succeeded
4. `aix.credentials.issue` returned a setup bundle with the server
   `runtime_public_base_url`
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
4. install `aix` through `POST /api/servers/:serverId/apps/aix/install`
5. wait until install status is `installed`
6. mint a runtime token for the selected server
7. call `aix.credentials.issue`
8. run local `aix connect --url <runtime_public_base_url> --token <aix_client_token>`
9. run local `aix push --mode backfill`
10. verify sources, runs, and imported sessions through runtime operations
11. enable recurring sync locally

Reference automation:

- `/Users/tyler/nexus/home/projects/nexus/apps/aix/scripts/hosted-published-server-drill.mjs`

---

## Notes

1. Human control uses frontdoor.
2. Machine upload uses the tenant runtime public base URL from the setup bundle.
3. Direct SSH to the frontdoor host or tenant host is not required for a
   successful drill.
4. Host-level inspection is optional supporting evidence, not the primary
   validation path.
