import fs from "node:fs";
import path from "node:path";
import { gunzipSync } from "node:zlib";
import { createHash, randomUUID } from "node:crypto";
import type { NexAppMethodHandler } from "../../../../nex/src/apps/context.js";
import type {
  AixCadence,
  AixMode,
  AixPreset,
  AixRunStatus,
  AixSourceStatus,
  AixTriggerKind,
} from "./store.js";
import {
  beginRun,
  beginUpload,
  completeRun,
  completeUpload,
  computeSourceHealth,
  createOrUpdateDevice,
  findCredentialById,
  findRunById,
  findSourceById,
  findSourceByInstallId,
  findUploadById,
  insertCredential,
  listReceivedChunkIndexes,
  listCredentials,
  listRuns,
  listSourceProviders,
  listSources,
  listUploadsForRun,
  openAixControlDb,
  updateUploadProgress,
  type AixCredentialRecord,
  updateCredentialRevocation,
  updateSource,
  upsertSource,
  upsertSourceProviders,
} from "./store.js";

const AIX_OPERATIONS_PATH = "/runtime/operations/";
const AIX_ARCHIVE_WORKSPACE_NAME = "AIX Archive";
const AIX_DEFAULT_SCOPES = [
  "core.apps.aix.sources.register.write",
  "core.apps.aix.runs.begin.write",
  "core.apps.aix.runs.complete.write",
  "core.apps.aix.uploads.begin.write",
  "core.apps.aix.uploads.chunk.write",
  "core.apps.aix.uploads.status.read",
  "core.apps.aix.uploads.complete.write",
  "core.apps.aix.sources.get.read",
  "core.workspaces.read",
  "core.workspaces.write",
  "core.agents.sessions.import.admin",
  "core.agents.sessions.imports.read",
] as const;

function requireCurrentEntityId(ctx: Parameters<NexAppMethodHandler>[0]): string {
  const entityId = String(ctx.user.userId ?? "").trim();
  if (!entityId) {
    throw new Error("authenticated entity id is required");
  }
  return entityId;
}

function asRecord(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return {};
  }
  return value as Record<string, unknown>;
}

function asNonEmptyString(value: unknown, field: string): string {
  if (typeof value !== "string") {
    throw new Error(`${field} is required`);
  }
  const trimmed = value.trim();
  if (!trimmed) {
    throw new Error(`${field} is required`);
  }
  return trimmed;
}

function asOptionalString(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

function asOptionalNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? Math.max(0, Math.floor(value)) : null;
}

function asStringArray(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.map((entry) => (typeof entry === "string" ? entry.trim() : "")).filter(Boolean);
}

function asPreset(value: unknown): AixPreset {
  if (value === "offboarding" || value === "ongoing") {
    return value;
  }
  throw new Error("preset must be \"offboarding\" or \"ongoing\"");
}

function asMode(value: unknown, fallback: AixMode = "backfill"): AixMode {
  if (value === undefined || value === null) {
    return fallback;
  }
  if (value === "backfill" || value === "incremental" || value === "live") {
    return value;
  }
  throw new Error("mode must be one of backfill, incremental, live");
}

function asCadence(value: unknown, fallback: AixCadence = "manual"): AixCadence {
  if (value === undefined || value === null) {
    return fallback;
  }
  if (
    value === "manual" ||
    value === "five_minutes" ||
    value === "ten_minutes" ||
    value === "daily" ||
    value === "live"
  ) {
    return value;
  }
  throw new Error("cadence must be one of manual, five_minutes, ten_minutes, daily, live");
}

function asSourceStatus(value: unknown): AixSourceStatus {
  if (value === "active" || value === "paused" || value === "revoked" || value === "error") {
    return value;
  }
  throw new Error("status must be one of active, paused, revoked, error");
}

function asUpdatableSourceStatus(value: unknown): Exclude<AixSourceStatus, "error"> {
  if (value === "active" || value === "paused" || value === "revoked") {
    return value;
  }
  throw new Error("status must be one of active, paused, revoked");
}

function asRunStatus(value: unknown): AixRunStatus {
  if (value === "running" || value === "completed" || value === "completed_empty" || value === "failed" || value === "partial") {
    return value;
  }
  throw new Error("run status must be one of running, completed, completed_empty, failed, partial");
}

function asTriggerKind(value: unknown): AixTriggerKind {
  if (value === "manual" || value === "scheduled" || value === "live" || value === "resume") {
    return value;
  }
  throw new Error("triggerKind must be one of manual, scheduled, live, resume");
}

function defaultModeForPreset(preset: AixPreset): AixMode {
  return "backfill";
}

function defaultCadenceForPreset(preset: AixPreset): AixCadence {
  return preset === "ongoing" ? "daily" : "manual";
}

function buildCredentialLabel(params: { entityId: string; preset: AixPreset; label: string | null }): string {
  if (params.label) {
    return params.label;
  }
  const compact = new Date().toISOString().replace(/[-:.TZ]/g, "").slice(0, 14);
  return `aix:${params.entityId}:${params.preset}:${compact}`;
}

type ImportedBatchItem = Record<string, unknown>;

async function resolveRuntimeBaseUrl(
  ctx: Parameters<NexAppMethodHandler>[0],
  override: unknown,
): Promise<string> {
  const explicit = asOptionalString(override);
  if (explicit) {
    return explicit.replace(/\/$/, "");
  }
  const configResponse = await ctx.nex.config.get({});
  const config = asRecord(configResponse.payload);
  const runtime = asRecord(config.runtime);
  const port = typeof runtime.port === "number" && Number.isFinite(runtime.port) ? runtime.port : 18789;
  const tls = asRecord(runtime.tls);
  const protocol = tls.enabled === true ? "https" : "http";
  return `${protocol}://127.0.0.1:${port}`;
}

async function resolveHostedRuntimeBaseUrl(
  ctx: Parameters<NexAppMethodHandler>[0],
): Promise<string | null> {
  try {
    const response = await ctx.nex.productControlPlane.call({
      operation: "aix.hostedContext.get",
      payload: {},
    });
    const result = asRecord(asRecord(response.payload).result);
    const runtimePublicBaseUrl = asOptionalString(result.runtime_public_base_url);
    if (!runtimePublicBaseUrl) {
      return null;
    }
    return runtimePublicBaseUrl.replace(/\/$/, "");
  } catch {
    return null;
  }
}

async function resolveSetupRuntimeBaseUrl(
  ctx: Parameters<NexAppMethodHandler>[0],
  override: unknown,
): Promise<string> {
  const explicit = asOptionalString(override);
  if (explicit) {
    return explicit.replace(/\/$/, "");
  }
  const hosted = await resolveHostedRuntimeBaseUrl(ctx);
  if (hosted) {
    return hosted;
  }
  return resolveRuntimeBaseUrl(ctx, null);
}

function buildSetupBundle(params: {
  runtimeBaseUrl: string;
  token: string;
  preset: AixPreset;
  defaultMode: AixMode;
  defaultCadence: AixCadence;
}): { runtimeBaseUrl: string; token: string; commands: string[]; promptText: string } {
  const commands = [
    "brew install Napageneral/tap/aix || go install github.com/Napageneral/aix/cmd/aix@latest",
    "aix init",
    `aix connect --url ${shellEscape(params.runtimeBaseUrl)} --token ${shellEscape(params.token)}`,
    `aix push --mode ${params.defaultMode}`,
  ];
  if (params.preset === "ongoing") {
    commands.push(`aix daemon enable --cadence ${params.defaultCadence}`);
  }
  const promptLines = [
    "Install AIX, connect it to the provided Nex runtime, and sync this engineer's local agent sessions.",
    "",
    "1. Install AIX with Homebrew or `go install github.com/Napageneral/aix/cmd/aix@latest`.",
    "2. Run `aix init`.",
    `3. Run \`aix connect --url ${params.runtimeBaseUrl} --token ${params.token}\`.`,
    `4. Run \`aix push --mode ${params.defaultMode}\`.`,
  ];
  if (params.preset === "ongoing") {
    promptLines.push(`5. Run \`aix daemon enable --cadence ${params.defaultCadence}\`.`);
    promptLines.push("6. Report the final AIX summary and confirm the daemon was enabled.");
  } else {
    promptLines.push("5. Report the final AIX summary and confirm the upload completed.");
  }
  return {
    runtimeBaseUrl: params.runtimeBaseUrl,
    token: params.token,
    commands,
    promptText: promptLines.join("\n"),
  };
}

function shellEscape(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

function credentialView(record: AixCredentialRecord) {
  return {
    id: record.id,
    entityId: record.entityId,
    tokenId: record.tokenId,
    label: record.label,
    expiresAt: record.expiresAt,
    revokedAt: record.revokedAt,
    createdAt: record.createdAt,
    lastUsedAt: record.lastUsedAt,
    preset: record.preset,
  };
}

function encodeCursor(value: { updatedAt: number; sessionKey: string }): string {
  return Buffer.from(JSON.stringify(value), "utf8").toString("base64url");
}

function decodeCursor(value: unknown): { updatedAt: number; sessionKey: string } | null {
  if (typeof value !== "string" || !value.trim()) {
    return null;
  }
  try {
    const parsed = JSON.parse(Buffer.from(value, "base64url").toString("utf8")) as Record<string, unknown>;
    if (typeof parsed.updatedAt !== "number" || typeof parsed.sessionKey !== "string") {
      return null;
    }
    return {
      updatedAt: parsed.updatedAt,
      sessionKey: parsed.sessionKey,
    };
  } catch {
    return null;
  }
}

function archiveWorkspacePath(dataDir: string): string {
  return path.join(dataDir, "archive-workspace");
}

function uploadSpoolDir(dataDir: string, uploadId: string): string {
  return path.join(dataDir, "spool", "uploads", uploadId);
}

function uploadChunkPath(dataDir: string, uploadId: string, chunkIndex: number): string {
  return path.join(uploadSpoolDir(dataDir, uploadId), `chunk-${String(chunkIndex).padStart(6, "0")}`);
}

function computeSpoolBytes(spoolDir: string): number {
  return listReceivedChunkIndexes(spoolDir).reduce((total, index) => {
    const chunkPath = path.join(spoolDir, `chunk-${String(index).padStart(6, "0")}`);
    try {
      return total + fs.statSync(chunkPath).size;
    } catch {
      return total;
    }
  }, 0);
}

function requireSourceForEntity(sourceId: string, entityId: string, dataDir: string) {
  const db = openAixControlDb(dataDir);
  try {
    const source = findSourceById(db, sourceId);
    if (!source || source.entityId !== entityId) {
      throw new Error(`unknown source id for entity: ${sourceId}`);
    }
    return source;
  } finally {
    db.close();
  }
}

function requireRunForSource(runId: string, sourceId: string, dataDir: string) {
  const db = openAixControlDb(dataDir);
  try {
    const run = findRunById(db, runId);
    if (!run || run.sourceId !== sourceId) {
      throw new Error(`unknown run id for source: ${runId}`);
    }
    return run;
  } finally {
    db.close();
  }
}

async function ensureArchiveWorkspace(
  ctx: Parameters<NexAppMethodHandler>[0],
): Promise<{ id: string; name: string; path: string }> {
  const listed = await ctx.nex.workspaces.list({
    namePattern: AIX_ARCHIVE_WORKSPACE_NAME,
  });
  const listedPayload = asRecord(listed.payload);
  const workspaces = Array.isArray(listedPayload.workspaces)
    ? listedPayload.workspaces.filter(
        (entry): entry is Record<string, unknown> =>
          !!entry && typeof entry === "object" && !Array.isArray(entry),
      )
    : [];
  const existing = workspaces.find(
    (workspace) => asOptionalString(workspace.name) === AIX_ARCHIVE_WORKSPACE_NAME,
  );
  if (existing) {
    const id = asOptionalString(existing.id);
    const existingPath = asOptionalString(existing.path);
    if (!id || !existingPath) {
      throw new Error("workspaces.list returned invalid AIX Archive workspace");
    }
    fs.mkdirSync(existingPath, { recursive: true });
    return {
      id,
      name: AIX_ARCHIVE_WORKSPACE_NAME,
      path: existingPath,
    };
  }

  const workspacePath = archiveWorkspacePath(ctx.app.dataDir);
  fs.mkdirSync(workspacePath, { recursive: true });
  try {
    const created = await ctx.nex.workspaces.create({
      name: AIX_ARCHIVE_WORKSPACE_NAME,
      path: workspacePath,
    });
    const id = asOptionalString(asRecord(created.payload).id);
    if (!id) {
      throw new Error("workspaces.create did not return a workspace id");
    }
    return {
      id,
      name: AIX_ARCHIVE_WORKSPACE_NAME,
      path: workspacePath,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (!/workspace name already exists/i.test(message)) {
      throw error;
    }
    const listedAfterConflict = await ctx.nex.workspaces.list({
      namePattern: AIX_ARCHIVE_WORKSPACE_NAME,
    });
    const conflictWorkspaces = Array.isArray(asRecord(listedAfterConflict.payload).workspaces)
      ? asRecord(listedAfterConflict.payload).workspaces.filter(
          (entry): entry is Record<string, unknown> =>
            !!entry && typeof entry === "object" && !Array.isArray(entry),
        )
      : [];
    const afterConflict = conflictWorkspaces.find(
      (workspace) => asOptionalString(workspace.name) === AIX_ARCHIVE_WORKSPACE_NAME,
    );
    const id = afterConflict ? asOptionalString(afterConflict.id) : null;
    const existingPath = afterConflict ? asOptionalString(afterConflict.path) : null;
    if (!id || !existingPath) {
      throw error;
    }
    fs.mkdirSync(existingPath, { recursive: true });
    return {
      id,
      name: AIX_ARCHIVE_WORKSPACE_NAME,
      path: existingPath,
    };
  }
}

async function queryImportedSessions(
  ctx: Parameters<NexAppMethodHandler>[0],
  params: {
    workspaceId: string;
    entityId?: string;
    sourceId?: string;
    provider?: string;
    limit: number;
    cursor: { updatedAt: number; sessionKey: string } | null;
  },
): Promise<{
  items: Array<{
    sessionKey: string;
    sourceEntityId: string;
    aixSourceId: string;
    sourceProvider: string;
    sourceSessionId: string;
    updatedAt: number;
    title: string | null;
    workspaceId: string | null;
  }>;
  nextCursor?: string;
}> {
  const response = await ctx.nex.agents.sessions.imports.list({
    workspaceId: params.workspaceId,
    sourceEntityId: params.entityId,
    aixSourceId: params.sourceId,
    sourceProvider: params.provider,
    limit: params.limit,
    cursor: params.cursor ? encodeCursor(params.cursor) : undefined,
  });
  const responsePayload = asRecord(response.payload);
  const rows = Array.isArray(responsePayload.items)
    ? responsePayload.items.filter(
        (entry): entry is Record<string, unknown> =>
          !!entry && typeof entry === "object" && !Array.isArray(entry),
      )
    : [];
  return {
    items: rows.map((row) => ({
      sessionKey: String(row.sessionKey ?? ""),
      sourceEntityId: String(row.sourceEntityId ?? ""),
      aixSourceId: String(row.aixSourceId ?? ""),
      sourceProvider: String(row.sourceProvider ?? ""),
      sourceSessionId: String(row.sourceSessionId ?? ""),
      updatedAt: typeof row.updatedAt === "number" ? row.updatedAt : 0,
      title: asOptionalString(row.title),
      workspaceId: asOptionalString(row.workspaceId),
    })),
    ...(typeof responsePayload.nextCursor === "string" && responsePayload.nextCursor.trim()
      ? { nextCursor: responsePayload.nextCursor }
      : {}),
  };
}

function asPositiveInteger(value: unknown, field: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${field} must be a positive integer`);
  }
  const normalized = Math.floor(value);
  if (normalized < 1) {
    throw new Error(`${field} must be a positive integer`);
  }
  return normalized;
}

function asNonNegativeInteger(value: unknown, field: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  const normalized = Math.floor(value);
  if (normalized < 0) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  return normalized;
}

function asLimit(value: unknown, fallback = 50, max = 200): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return fallback;
  }
  return Math.max(1, Math.min(max, Math.floor(value)));
}

function sha256Hex(value: string): string {
  return createHash("sha256").update(value, "utf8").digest("hex");
}

function normalizeImportStats(value: unknown): { imported: number; upserted: number; skipped: number; failed: number } | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const record = value as Record<string, unknown>;
  return {
    imported: typeof record.imported === "number" ? record.imported : 0,
    upserted: typeof record.upserted === "number" ? record.upserted : 0,
    skipped: typeof record.skipped === "number" ? record.skipped : 0,
    failed: typeof record.failed === "number" ? record.failed : 0,
  };
}

function inflateBatchPayload(encodedPayload: string): ImportedBatchItem[] {
  let parsed: unknown;
  try {
    const compressed = Buffer.from(encodedPayload, "base64");
    const json = gunzipSync(compressed).toString("utf8");
    parsed = JSON.parse(json);
  } catch (error) {
    throw new Error(`invalid upload payload: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("invalid upload payload: root object required");
  }
  const record = parsed as Record<string, unknown>;
  if (record.format !== "aix-session-batch-v1") {
    throw new Error("invalid upload payload: unsupported format");
  }
  if (record.source !== "aix") {
    throw new Error("invalid upload payload: source must be aix");
  }
  if (!Array.isArray(record.items) || record.items.length === 0) {
    throw new Error("invalid upload payload: items must be a non-empty array");
  }
  return record.items as ImportedBatchItem[];
}

function assembleUploadPayload(dataDir: string, uploadId: string, chunkTotal: number): {
  receivedChunkIndexes: number[];
  encodedPayload: string;
  bytesReceived: number;
} {
  const spoolDir = uploadSpoolDir(dataDir, uploadId);
  const receivedChunkIndexes = listReceivedChunkIndexes(spoolDir);
  if (receivedChunkIndexes.length !== chunkTotal) {
    throw new Error("upload_incomplete");
  }
  for (let index = 0; index < chunkTotal; index += 1) {
    if (receivedChunkIndexes[index] !== index) {
      throw new Error("upload_incomplete");
    }
  }
  const encodedPayload = receivedChunkIndexes
    .map((index) => fs.readFileSync(uploadChunkPath(dataDir, uploadId, index), "utf8"))
    .join("");
  return {
    receivedChunkIndexes,
    encodedPayload,
    bytesReceived: computeSpoolBytes(spoolDir),
  };
}

const issueCredential: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const preset = asPreset(params.preset);
  const entityId = asNonEmptyString(params.entityId, "entityId");
  const issuedByEntityId = requireCurrentEntityId(ctx);
  const label = asOptionalString(params.label);
  const expiresAt = asOptionalNumber(params.expiresAt);
  const providerAllowlist = asStringArray(params.providerAllowlist);
  const defaultMode = asMode(params.defaultMode, defaultModeForPreset(preset));
  const defaultCadence = asCadence(params.defaultCadence, defaultCadenceForPreset(preset));
  const runtimeBaseUrl = await resolveSetupRuntimeBaseUrl(ctx, params.runtimeBaseUrl);
  const tokenLabel = buildCredentialLabel({ entityId, preset, label });

  const created = await ctx.nex.auth.tokens.create({
    entityId,
    role: "operator",
    scopes: [...AIX_DEFAULT_SCOPES],
    label: tokenLabel,
    expiresAt,
  });

  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const createdPayload = asRecord(created.payload);
    const createdCredential = asRecord(createdPayload.credential);
    const credential = insertCredential(db, {
      tokenId: String(createdCredential.id ?? ""),
      entityId,
      purpose: "client",
      label: tokenLabel,
      preset,
      issuedByEntityId,
      createdAt: typeof createdCredential.createdAt === "number" ? createdCredential.createdAt : 0,
      firstUsedAt:
        typeof createdCredential.lastUsedAt === "number" ? createdCredential.lastUsedAt : null,
      lastUsedAt:
        typeof createdCredential.lastUsedAt === "number" ? createdCredential.lastUsedAt : null,
      expiresAt:
        typeof createdCredential.expiresAt === "number" ? createdCredential.expiresAt : null,
      revokedAt:
        typeof createdCredential.revokedAt === "number" ? createdCredential.revokedAt : null,
      metadata: {
        providerAllowlist,
        defaultMode,
        defaultCadence,
        runtimeBaseUrl,
      },
    });
    const setupBundle = buildSetupBundle({
      runtimeBaseUrl,
      token: String(createdPayload.token ?? ""),
      preset,
      defaultMode,
      defaultCadence,
    });
    return {
      ok: true,
      credential: credentialView(credential),
      token: String(createdPayload.token ?? ""),
      setupBundle,
    };
  } finally {
    db.close();
  }
};

const listCredentialRecords: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const credentials = listCredentials(db, {
      entityId: asOptionalString(params.entityId) ?? undefined,
      includeRevoked: params.includeRevoked === true,
      includeExpired: params.includeExpired === true,
      limit: typeof params.limit === "number" ? params.limit : undefined,
    }).map((record) => ({
      id: record.id,
      entityId: record.entityId,
      label: record.label,
      preset: record.preset,
      createdAt: record.createdAt,
      lastUsedAt: record.lastUsedAt,
      expiresAt: record.expiresAt,
      revokedAt: record.revokedAt,
    }));
    return { credentials };
  } finally {
    db.close();
  }
};

const revokeCredential: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const id = asNonEmptyString(params.id, "id");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const existing = findCredentialById(db, id);
    if (!existing) {
      throw new Error(`unknown credential id: ${id}`);
    }
    const result = await ctx.nex.auth.tokens.revoke({ id: existing.tokenId });
    if (asRecord(result.payload).revoked === true) {
      updateCredentialRevocation(db, { id, revokedAt: Date.now() });
    }
    return { ok: true, revoked: asRecord(result.payload).revoked === true };
  } finally {
    db.close();
  }
};

const rotateCredential: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const id = asNonEmptyString(params.id, "id");
  const overrideLabel = asOptionalString(params.label);
  const expiresAt = asOptionalNumber(params.expiresAt);
  const runtimeBaseUrlOverride = params.runtimeBaseUrl;
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const existing = findCredentialById(db, id);
    if (!existing) {
      throw new Error(`unknown credential id: ${id}`);
    }
    const metadata = existing.metadata ?? {};
    const defaultMode = asMode(metadata.defaultMode, defaultModeForPreset(existing.preset));
    const defaultCadence = asCadence(metadata.defaultCadence, defaultCadenceForPreset(existing.preset));
    const runtimeBaseUrl = await resolveSetupRuntimeBaseUrl(
      ctx,
      runtimeBaseUrlOverride ?? metadata.runtimeBaseUrl,
    );
    const label = buildCredentialLabel({
      entityId: existing.entityId,
      preset: existing.preset,
      label: overrideLabel ?? existing.label,
    });
    const rotated = await ctx.nex.auth.tokens.rotate({
      id: existing.tokenId,
      role: "operator",
      scopes: [...AIX_DEFAULT_SCOPES],
      label,
      expiresAt,
    });
    const rotatedPayload = asRecord(rotated.payload);
    const rotatedCredential = asRecord(rotatedPayload.credential);
    updateCredentialRevocation(db, { id: existing.id, revokedAt: Date.now() });
    const credential = insertCredential(db, {
      tokenId: String(rotatedCredential.id ?? ""),
      entityId: existing.entityId,
      purpose: existing.purpose,
      label,
      preset: existing.preset,
      issuedByEntityId: requireCurrentEntityId(ctx),
      createdAt: typeof rotatedCredential.createdAt === "number" ? rotatedCredential.createdAt : 0,
      firstUsedAt:
        typeof rotatedCredential.lastUsedAt === "number" ? rotatedCredential.lastUsedAt : null,
      lastUsedAt:
        typeof rotatedCredential.lastUsedAt === "number" ? rotatedCredential.lastUsedAt : null,
      expiresAt:
        typeof rotatedCredential.expiresAt === "number" ? rotatedCredential.expiresAt : null,
      revokedAt:
        typeof rotatedCredential.revokedAt === "number" ? rotatedCredential.revokedAt : null,
      metadata: {
        ...metadata,
        defaultMode,
        defaultCadence,
        runtimeBaseUrl,
      },
    });
    return {
      ok: true,
      previousId: existing.id,
      credential: credentialView(credential),
      token: String(rotatedPayload.token ?? ""),
      setupBundle: buildSetupBundle({
        runtimeBaseUrl,
        token: String(rotatedPayload.token ?? ""),
        preset: existing.preset,
        defaultMode,
        defaultCadence,
      }),
    };
  } finally {
    db.close();
  }
};

const listEntityRecords: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const result = await ctx.nex.entities.list({
    limit: asLimit(params.limit, 100, 500),
    offset: typeof params.offset === "number" ? Math.max(0, Math.floor(params.offset)) : 0,
    type: asOptionalString(params.type),
    is_user:
      typeof params.isUser === "boolean"
        ? params.isUser
        : true,
    merged: false,
  });
  const entities = Array.isArray(asRecord(result.payload).entities)
    ? asRecord(result.payload).entities.filter(
        (entry): entry is Record<string, unknown> =>
          !!entry && typeof entry === "object" && !Array.isArray(entry),
      )
    : [];
  return {
    entities: entities.map((entity) => ({
      id: String(entity.id ?? ""),
      name: asOptionalString(entity.name) ?? String(entity.id ?? ""),
      type: asOptionalString(entity.type),
      normalized: asOptionalString(entity.normalized),
      isUser: entity.is_user === true || entity.is_user === 1,
      isAgent: entity.is_agent === true || entity.is_agent === 1,
      origin: asOptionalString(entity.origin),
      createdAt: typeof entity.created_at === "number" ? entity.created_at : null,
    })),
  };
};

const registerSource: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const installId = asNonEmptyString(params.installId, "installId");
  const clientDeviceId = asNonEmptyString(params.clientDeviceId, "clientDeviceId");
  const clientVersion = asNonEmptyString(params.clientVersion, "clientVersion");
  const providersRaw = Array.isArray(params.providers) ? params.providers : [];
  if (providersRaw.length === 0) {
    throw new Error("providers must contain at least one provider");
  }
  const providers = providersRaw.map((entry) => {
    const record = asRecord(entry);
    return {
      provider: asNonEmptyString(record.provider, "providers[].provider"),
      enabled: record.enabled !== false,
    };
  });
  const now = Date.now();
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const existing = findSourceByInstallId(db, installId);
    if (existing && existing.entityId !== entityId) {
      throw new Error(`installId already belongs to another entity: ${installId}`);
    }
    const device = createOrUpdateDevice(db, {
      entityId,
      clientDeviceId,
      hostname: asOptionalString(params.hostname),
      platform: asOptionalString(params.platform),
      arch: asOptionalString(params.arch),
      osVersion: asOptionalString(params.osVersion),
      metadata: null,
      now,
    });
    const source = upsertSource(db, {
      entityId,
      installId,
      currentDeviceId: device.id,
      label: asOptionalString(params.label),
      status: existing?.status ?? "active",
      defaultMode: asMode(params.defaultMode, existing?.defaultMode ?? "backfill"),
      expectedCadence: asCadence(params.expectedCadence, existing?.expectedCadence ?? "manual"),
      clientVersion,
      metadata: null,
      now,
    });
    upsertSourceProviders(db, {
      sourceId: source.id,
      providers,
      now,
    });
    return {
      ok: true,
      source: {
        id: source.id,
        entityId: source.entityId,
        installId: source.installId,
        currentDeviceId: source.currentDeviceId,
        status: source.status,
        defaultMode: source.defaultMode,
        expectedCadence: source.expectedCadence,
      },
      runtime: {
        baseUrl: await resolveSetupRuntimeBaseUrl(ctx, null),
        operationsPath: AIX_OPERATIONS_PATH,
      },
    };
  } finally {
    db.close();
  }
};

const listSourceRecords: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const sources = listSources(db, {
      entityId: asOptionalString(params.entityId) ?? undefined,
      status: params.status ? asSourceStatus(params.status) : undefined,
      provider: asOptionalString(params.provider) ?? undefined,
      limit: typeof params.limit === "number" ? params.limit : undefined,
    }).map((source) => ({
      id: source.id,
      entityId: source.entityId,
      installId: source.installId,
      currentDeviceId: source.currentDeviceId,
      label: source.label,
      status: source.status,
      defaultMode: source.defaultMode,
      expectedCadence: source.expectedCadence,
      lastSeenAt: source.lastSeenAt,
      lastSuccessAt: source.lastSuccessAt,
      lastFailureAt: source.lastFailureAt,
      lastRunId: source.lastRunId,
      lastError: source.lastError,
      clientVersion: source.clientVersion,
      providers: source.providers.map((provider) => ({
        provider: provider.provider,
        enabled: provider.enabled,
        lastSeenAt: provider.lastSeenAt,
      })),
      health: computeSourceHealth(source),
    }));
    return { sources };
  } finally {
    db.close();
  }
};

const getSource: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const source = findSourceById(db, sourceId);
    if (!source) {
      throw new Error(`unknown source id: ${sourceId}`);
    }
    const providers = listSourceProviders(db, source.id);
    const device = source.currentDeviceId
      ? db.prepare("SELECT * FROM aix_devices WHERE id = ? LIMIT 1").get(source.currentDeviceId) as Record<string, unknown> | undefined
      : undefined;
    return {
      source: {
        id: source.id,
        entityId: source.entityId,
        installId: source.installId,
        currentDeviceId: source.currentDeviceId,
        label: source.label,
        status: source.status,
        defaultMode: source.defaultMode,
        expectedCadence: source.expectedCadence,
        firstSeenAt: source.firstSeenAt,
        lastSeenAt: source.lastSeenAt,
        lastSuccessAt: source.lastSuccessAt,
        lastFailureAt: source.lastFailureAt,
        lastRunId: source.lastRunId,
        lastError: source.lastError,
        clientVersion: source.clientVersion,
        metadata: source.metadata,
      },
      device: device
        ? {
            id: String(device.id ?? ""),
            clientDeviceId: String(device.client_device_id ?? ""),
            hostname: asOptionalString(device.hostname),
            platform: asOptionalString(device.platform),
            arch: asOptionalString(device.arch),
            osVersion: asOptionalString(device.os_version),
            lastSeenAt: typeof device.last_seen_at === "number" ? device.last_seen_at : null,
          }
        : null,
      providers: providers.map((provider) => ({
        provider: provider.provider,
        enabled: provider.enabled,
        firstSeenAt: provider.firstSeenAt,
        lastSeenAt: provider.lastSeenAt,
      })),
    };
  } finally {
    db.close();
  }
};

const updateSourceRecord: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const updated = updateSource(db, {
      sourceId,
      label: params.label !== undefined ? asOptionalString(params.label) : undefined,
      status: params.status !== undefined ? asUpdatableSourceStatus(params.status) : undefined,
      defaultMode: params.defaultMode !== undefined ? asMode(params.defaultMode) : undefined,
      expectedCadence: params.expectedCadence !== undefined ? asCadence(params.expectedCadence) : undefined,
    });
    if (!updated) {
      throw new Error(`unknown source id: ${sourceId}`);
    }
    return { ok: true, sourceId: updated.id };
  } finally {
    db.close();
  }
};

const beginRunHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const clientRunId = asNonEmptyString(params.clientRunId, "clientRunId");
  const clientVersion = asNonEmptyString(params.clientVersion, "clientVersion");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const source = findSourceById(db, sourceId);
    if (!source || source.entityId !== entityId) {
      throw new Error(`unknown source id for entity: ${sourceId}`);
    }
    const providerSummary = Array.isArray(params.providerSummary)
      ? params.providerSummary.map((entry) => {
          const record = asRecord(entry);
          return {
            provider: asNonEmptyString(record.provider, "providerSummary[].provider"),
            sessionsSeen: typeof record.sessionsSeen === "number" ? Math.max(0, Math.floor(record.sessionsSeen)) : 0,
            sessionsChanged:
              typeof record.sessionsChanged === "number" ? Math.max(0, Math.floor(record.sessionsChanged)) : 0,
          };
        })
      : [];
    const result = beginRun(db, {
      source,
      clientRunId,
      triggerKind: asTriggerKind(params.triggerKind),
      runMode: asMode(params.runMode),
      clientVersion,
      providerSummary,
      checkpoint: params.checkpoint && typeof params.checkpoint === "object" && !Array.isArray(params.checkpoint)
        ? (params.checkpoint as Record<string, unknown>)
        : null,
      now: Date.now(),
    });
    return {
      ok: true,
      run: {
        id: result.run.id,
        sourceId: result.run.sourceId,
        entityId: result.run.entityId,
        clientRunId: result.run.clientRunId,
        status: result.run.status,
        startedAt: result.run.startedAt,
      },
      existing: result.existing,
    };
  } finally {
    db.close();
  }
};

const completeRunHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const runId = asNonEmptyString(params.runId, "runId");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const source = findSourceById(db, sourceId);
    if (!source || source.entityId !== entityId) {
      throw new Error(`unknown source id for entity: ${sourceId}`);
    }
    const run = completeRun(db, {
      source,
      runId,
      status: asRunStatus(params.status),
      sessionsSeen: typeof params.sessionsSeen === "number" ? params.sessionsSeen : 0,
      sessionsChanged: typeof params.sessionsChanged === "number" ? params.sessionsChanged : 0,
      imported: typeof params.imported === "number" ? params.imported : 0,
      upserted: typeof params.upserted === "number" ? params.upserted : 0,
      skipped: typeof params.skipped === "number" ? params.skipped : 0,
      failed: typeof params.failed === "number" ? params.failed : 0,
      bytesUploaded: typeof params.bytesUploaded === "number" ? params.bytesUploaded : 0,
      uploadCount: typeof params.uploadCount === "number" ? params.uploadCount : 0,
      checkpoint: params.checkpoint && typeof params.checkpoint === "object" && !Array.isArray(params.checkpoint)
        ? (params.checkpoint as Record<string, unknown>)
        : null,
      errorCode: asOptionalString(params.errorCode),
      errorMessage: asOptionalString(params.errorMessage),
      now: Date.now(),
    });
    if (!run) {
      throw new Error(`unknown run id for source: ${runId}`);
    }
    return { ok: true, runId: run.id };
  } finally {
    db.close();
  }
};

const listRunRecords: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const runs = listRuns(db, {
      entityId: asOptionalString(params.entityId) ?? undefined,
      sourceId: asOptionalString(params.sourceId) ?? undefined,
      status: params.status ? asRunStatus(params.status) : undefined,
      limit: typeof params.limit === "number" ? params.limit : undefined,
    }).map((run) => ({
      id: run.id,
      sourceId: run.sourceId,
      entityId: run.entityId,
      clientRunId: run.clientRunId,
      triggerKind: run.triggerKind,
      runMode: run.runMode,
      status: run.status,
      startedAt: run.startedAt,
      completedAt: run.completedAt,
      sessionsSeen: run.sessionsSeen,
      sessionsChanged: run.sessionsChanged,
      imported: run.imported,
      upserted: run.upserted,
      skipped: run.skipped,
      failed: run.failed,
      bytesUploaded: run.bytesUploaded,
      uploadCount: run.uploadCount,
      errorCode: run.errorCode,
      errorMessage: run.errorMessage,
    }));
    return { runs };
  } finally {
    db.close();
  }
};

const getRun: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const runId = asNonEmptyString(params.runId, "runId");
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const run = findRunById(db, runId);
    if (!run) {
      throw new Error(`unknown run id: ${runId}`);
    }
    return {
      run: {
        id: run.id,
        sourceId: run.sourceId,
        entityId: run.entityId,
        clientRunId: run.clientRunId,
        triggerKind: run.triggerKind,
        runMode: run.runMode,
        status: run.status,
        startedAt: run.startedAt,
        completedAt: run.completedAt,
        sessionsSeen: run.sessionsSeen,
        sessionsChanged: run.sessionsChanged,
        imported: run.imported,
        upserted: run.upserted,
        skipped: run.skipped,
        failed: run.failed,
        bytesUploaded: run.bytesUploaded,
        uploadCount: run.uploadCount,
        errorCode: run.errorCode,
        errorMessage: run.errorMessage,
        checkpoint: run.checkpoint,
      },
      uploads: listUploadsForRun(db, run.id),
    };
  } finally {
    db.close();
  }
};

const beginUploadHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const runId = asNonEmptyString(params.runId, "runId");
  const uploadId = asNonEmptyString(params.uploadId, "uploadId");
  const contentKind = asNonEmptyString(params.contentKind, "contentKind");
  if (contentKind !== "sessions.batch") {
    throw new Error("contentKind must be sessions.batch");
  }
  const payloadSha256 = asNonEmptyString(params.payloadSha256, "payloadSha256");
  const chunkTotal = asPositiveInteger(params.chunkTotal, "chunkTotal");
  const itemCount = asNonNegativeInteger(params.itemCount, "itemCount");
  const source = requireSourceForEntity(sourceId, entityId, ctx.app.dataDir);
  const run = requireRunForSource(runId, sourceId, ctx.app.dataDir);
  const spoolDir = uploadSpoolDir(ctx.app.dataDir, uploadId);
  fs.mkdirSync(spoolDir, { recursive: true });
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const existing = findUploadById(db, uploadId);
    if (existing) {
      if (existing.runId !== run.id || existing.sourceId !== source.id) {
        throw new Error(`upload id already belongs to another run: ${uploadId}`);
      }
      if (existing.payloadSha256 !== payloadSha256) {
        throw new Error("upload_payload_sha256_mismatch");
      }
      if (existing.chunkTotal !== chunkTotal) {
        throw new Error("upload_chunk_total_mismatch");
      }
      if (existing.contentKind !== contentKind) {
        throw new Error("upload_content_kind_mismatch");
      }
      if (existing.status === "completed") {
        return {
          ok: true,
          upload: {
            id: existing.id,
            status: existing.status,
            chunkTotal: existing.chunkTotal,
            receivedRanges: existing.receivedRanges,
            bytesReceived: existing.bytesReceived,
          },
        };
      }
    }
    beginUpload(db, {
      id: uploadId,
      runId: run.id,
      sourceId: source.id,
      contentKind,
      payloadSha256,
      chunkTotal,
      itemCount,
      now: Date.now(),
    });
    const receivedChunkIndexes = listReceivedChunkIndexes(spoolDir);
    const upload = updateUploadProgress(db, {
      uploadId,
      bytesReceived: computeSpoolBytes(spoolDir),
      receivedChunkIndexes,
      now: Date.now(),
    }) ?? findUploadById(db, uploadId);
    if (!upload) {
      throw new Error(`unknown upload id: ${uploadId}`);
    }
    return {
      ok: true,
      upload: {
        id: upload.id,
        status: upload.status,
        chunkTotal: upload.chunkTotal,
        receivedRanges: upload.receivedRanges,
        bytesReceived: upload.bytesReceived,
      },
    };
  } finally {
    db.close();
  }
};

const uploadChunkHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const runId = asNonEmptyString(params.runId, "runId");
  const uploadId = asNonEmptyString(params.uploadId, "uploadId");
  const chunkIndex = asNonNegativeInteger(params.chunkIndex, "chunkIndex");
  const chunkTotal = asPositiveInteger(params.chunkTotal, "chunkTotal");
  const payloadSha256 = asNonEmptyString(params.payloadSha256, "payloadSha256");
  const encoding = asNonEmptyString(params.encoding, "encoding");
  if (encoding !== "gzip+base64") {
    throw new Error("encoding must be gzip+base64");
  }
  const data = asNonEmptyString(params.data, "data");
  if (chunkIndex >= chunkTotal) {
    throw new Error("chunkIndex_out_of_range");
  }
  requireSourceForEntity(sourceId, entityId, ctx.app.dataDir);
  requireRunForSource(runId, sourceId, ctx.app.dataDir);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const upload = findUploadById(db, uploadId);
    if (!upload || upload.runId !== runId || upload.sourceId !== sourceId) {
      throw new Error(`unknown upload id for run: ${uploadId}`);
    }
    if (upload.payloadSha256 !== payloadSha256) {
      throw new Error("upload_payload_sha256_mismatch");
    }
    if (upload.chunkTotal !== chunkTotal) {
      throw new Error("upload_chunk_total_mismatch");
    }
    const chunkPath = uploadChunkPath(ctx.app.dataDir, uploadId, chunkIndex);
    fs.mkdirSync(path.dirname(chunkPath), { recursive: true });
    if (fs.existsSync(chunkPath)) {
      const existing = fs.readFileSync(chunkPath, "utf8");
      if (existing !== data) {
        throw new Error("chunk_payload_mismatch");
      }
    } else {
      fs.writeFileSync(chunkPath, data, "utf8");
    }
    const spoolDir = uploadSpoolDir(ctx.app.dataDir, uploadId);
    const updated = updateUploadProgress(db, {
      uploadId,
      bytesReceived: computeSpoolBytes(spoolDir),
      receivedChunkIndexes: listReceivedChunkIndexes(spoolDir),
      now: Date.now(),
    });
    if (!updated) {
      throw new Error(`unknown upload id: ${uploadId}`);
    }
    return {
      ok: true,
      uploadId: updated.id,
      status: updated.status,
      receivedRanges: updated.receivedRanges,
      bytesReceived: updated.bytesReceived,
    };
  } finally {
    db.close();
  }
};

const uploadStatusHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const runId = asNonEmptyString(params.runId, "runId");
  const uploadId = asNonEmptyString(params.uploadId, "uploadId");
  requireSourceForEntity(sourceId, entityId, ctx.app.dataDir);
  requireRunForSource(runId, sourceId, ctx.app.dataDir);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const upload = findUploadById(db, uploadId);
    if (!upload) {
      return {
        ok: true,
        uploadId,
        status: "missing",
        chunkTotal: null,
        receivedRanges: [],
        bytesReceived: 0,
      };
    }
    if (upload.runId !== runId || upload.sourceId !== sourceId) {
      throw new Error(`unknown upload id for run: ${uploadId}`);
    }
    const current =
      upload.status === "staging"
        ? updateUploadProgress(db, {
            uploadId,
            bytesReceived: computeSpoolBytes(uploadSpoolDir(ctx.app.dataDir, uploadId)),
            receivedChunkIndexes: listReceivedChunkIndexes(uploadSpoolDir(ctx.app.dataDir, uploadId)),
            now: Date.now(),
          }) ?? upload
        : upload;
    const result = normalizeImportStats(current.result);
    return {
      ok: true,
      uploadId: current.id,
      status: current.status,
      chunkTotal: current.chunkTotal,
      receivedRanges: current.receivedRanges,
      bytesReceived: current.bytesReceived,
      ...(result ? { result } : {}),
    };
  } finally {
    db.close();
  }
};

const completeUploadHandler: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const entityId = requireCurrentEntityId(ctx);
  const sourceId = asNonEmptyString(params.sourceId, "sourceId");
  const runId = asNonEmptyString(params.runId, "runId");
  const uploadId = asNonEmptyString(params.uploadId, "uploadId");
  const source = requireSourceForEntity(sourceId, entityId, ctx.app.dataDir);
  const run = requireRunForSource(runId, sourceId, ctx.app.dataDir);
  const db = openAixControlDb(ctx.app.dataDir);
  try {
    const existing = findUploadById(db, uploadId);
    if (!existing || existing.runId !== run.id || existing.sourceId !== source.id) {
      throw new Error(`unknown upload id for run: ${uploadId}`);
    }
    const existingResult = normalizeImportStats(existing.result);
    if (existing.status === "completed" && existingResult) {
      fs.rmSync(uploadSpoolDir(ctx.app.dataDir, uploadId), { recursive: true, force: true });
      return {
        ok: true,
        uploadId: existing.id,
        status: "completed",
        import: existingResult,
      };
    }

    let receivedChunkIndexes: number[] = [];
    let bytesReceived = 0;
    try {
      const assembled = assembleUploadPayload(ctx.app.dataDir, uploadId, existing.chunkTotal);
      receivedChunkIndexes = assembled.receivedChunkIndexes;
      bytesReceived = assembled.bytesReceived;
      if (sha256Hex(assembled.encodedPayload) !== existing.payloadSha256) {
        throw new Error("upload_payload_sha256_mismatch");
      }
      const items = inflateBatchPayload(assembled.encodedPayload);
      if (items.length !== existing.itemCount) {
        throw new Error("upload_item_count_mismatch");
      }
      const archiveWorkspace = await ensureArchiveWorkspace(ctx);
      const importResultResponse = await ctx.nex.agents.sessions.import.execute({
        source: "aix",
        runId: run.id,
        mode: run.runMode === "backfill" ? "backfill" : "tail",
        workspaceId: archiveWorkspace.id,
        sourceEntityId: source.entityId,
        aixSourceId: source.id,
        idempotencyKey: `aix-upload:${uploadId}`,
        items,
      });
      const importResult = asRecord(importResultResponse.payload);
        const result = {
          imported: typeof importResult.imported === "number" ? importResult.imported : 0,
          upserted: typeof importResult.upserted === "number" ? importResult.upserted : 0,
          skipped: typeof importResult.skipped === "number" ? importResult.skipped : 0,
          failed: typeof importResult.failed === "number" ? importResult.failed : 0,
        };
        completeUpload(db, {
          uploadId,
          status: "completed",
          bytesReceived,
          receivedChunkIndexes,
          result: {
            ...result,
            archiveWorkspaceId: archiveWorkspace.id,
          },
          now: Date.now(),
        });
        fs.rmSync(uploadSpoolDir(ctx.app.dataDir, uploadId), { recursive: true, force: true });
        return {
          ok: true,
          uploadId,
          status: "completed",
          import: result,
        };
    } catch (error) {
      const spoolDir = uploadSpoolDir(ctx.app.dataDir, uploadId);
      const finalIndexes = receivedChunkIndexes.length > 0 ? receivedChunkIndexes : listReceivedChunkIndexes(spoolDir);
      const finalBytes = bytesReceived > 0 ? bytesReceived : computeSpoolBytes(spoolDir);
      completeUpload(db, {
        uploadId,
        status: "failed",
        bytesReceived: finalBytes,
        receivedChunkIndexes: finalIndexes,
        errorCode: "upload_finalize_failed",
        errorMessage: error instanceof Error ? error.message : String(error),
        now: Date.now(),
      });
      throw error;
    }
  } finally {
    db.close();
  }
};

const listImportedSessions: NexAppMethodHandler = async (ctx) => {
  const params = asRecord(ctx.params);
  const archiveWorkspace = await ensureArchiveWorkspace(ctx);
  return await queryImportedSessions(ctx, {
    workspaceId: archiveWorkspace.id,
    entityId: asOptionalString(params.entityId) ?? undefined,
    sourceId: asOptionalString(params.sourceId) ?? undefined,
    provider: asOptionalString(params.provider) ?? undefined,
    limit: asLimit(params.limit),
    cursor: decodeCursor(params.cursor),
  });
};

export const handlers: Record<string, NexAppMethodHandler> = {
  "aix.credentials.issue": issueCredential,
  "aix.credentials.list": listCredentialRecords,
  "aix.credentials.revoke": revokeCredential,
  "aix.credentials.rotate": rotateCredential,
  "aix.entities.list": listEntityRecords,
  "aix.sources.register": registerSource,
  "aix.sources.list": listSourceRecords,
  "aix.sources.get": getSource,
  "aix.sources.update": updateSourceRecord,
  "aix.runs.begin": beginRunHandler,
  "aix.runs.complete": completeRunHandler,
  "aix.runs.list": listRunRecords,
  "aix.runs.get": getRun,
  "aix.uploads.begin": beginUploadHandler,
  "aix.uploads.chunk": uploadChunkHandler,
  "aix.uploads.status": uploadStatusHandler,
  "aix.uploads.complete": completeUploadHandler,
  "aix.imported-sessions.list": listImportedSessions,
};
