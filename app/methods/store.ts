import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

export type AixPreset = "offboarding" | "ongoing";
export type AixMode = "backfill" | "incremental" | "live";
export type AixCadence = "manual" | "five_minutes" | "ten_minutes" | "daily" | "live";
export type AixSourceStatus = "active" | "paused" | "revoked" | "error";
export type AixRunStatus = "running" | "completed" | "completed_empty" | "failed" | "partial";
export type AixTriggerKind = "manual" | "scheduled" | "live" | "resume";
export type AixUploadStatus = "staging" | "completed" | "failed";

export type AixCredentialRecord = {
  id: string;
  tokenId: string;
  entityId: string;
  purpose: string;
  label: string | null;
  preset: AixPreset;
  issuedByEntityId: string;
  createdAt: number;
  firstUsedAt: number | null;
  lastUsedAt: number | null;
  expiresAt: number | null;
  revokedAt: number | null;
  metadata: Record<string, unknown> | null;
};

export type AixDeviceRecord = {
  id: string;
  entityId: string;
  clientDeviceId: string;
  hostname: string | null;
  platform: string | null;
  arch: string | null;
  osVersion: string | null;
  metadata: Record<string, unknown> | null;
  firstSeenAt: number;
  lastSeenAt: number;
};

export type AixSourceRecord = {
  id: string;
  entityId: string;
  installId: string;
  currentDeviceId: string | null;
  label: string | null;
  status: AixSourceStatus;
  defaultMode: AixMode;
  expectedCadence: AixCadence;
  createdAt: number;
  firstSeenAt: number;
  lastSeenAt: number;
  lastSuccessAt: number | null;
  lastFailureAt: number | null;
  lastRunId: string | null;
  lastError: string | null;
  clientVersion: string | null;
  metadata: Record<string, unknown> | null;
};

export type AixSourceProviderRecord = {
  sourceId: string;
  provider: string;
  enabled: boolean;
  firstSeenAt: number;
  lastSeenAt: number;
  stats: Record<string, unknown> | null;
};

export type AixRunRecord = {
  id: string;
  sourceId: string;
  entityId: string;
  clientRunId: string;
  triggerKind: AixTriggerKind;
  runMode: AixMode;
  status: AixRunStatus;
  startedAt: number;
  completedAt: number | null;
  sessionsSeen: number;
  sessionsChanged: number;
  imported: number;
  upserted: number;
  skipped: number;
  failed: number;
  bytesUploaded: number;
  uploadCount: number;
  errorCode: string | null;
  errorMessage: string | null;
  checkpoint: Record<string, unknown> | null;
  metadata: Record<string, unknown> | null;
};

export type AixUploadRange = {
  start: number;
  end: number;
};

export type AixUploadRecord = {
  id: string;
  runId: string;
  sourceId: string;
  status: AixUploadStatus;
  contentKind: string;
  payloadSha256: string;
  chunkTotal: number;
  receivedRanges: AixUploadRange[];
  bytesReceived: number;
  itemCount: number;
  createdAt: number;
  updatedAt: number;
  completedAt: number | null;
  errorCode: string | null;
  errorMessage: string | null;
  result: Record<string, unknown> | null;
};

export type AixSourceHealth = "healthy" | "stale" | "failed" | "idle";

const AIX_CONTROL_DB_NAME = "aix-control.db";

const AIX_CONTROL_SCHEMA_SQL = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS aix_credentials (
  id TEXT PRIMARY KEY,
  token_id TEXT NOT NULL UNIQUE,
  entity_id TEXT NOT NULL,
  purpose TEXT NOT NULL,
  label TEXT,
  preset TEXT NOT NULL,
  issued_by_entity_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  first_used_at INTEGER,
  last_used_at INTEGER,
  expires_at INTEGER,
  revoked_at INTEGER,
  metadata_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_aix_credentials_entity_created ON aix_credentials(entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_aix_credentials_revoked_expires ON aix_credentials(revoked_at, expires_at);

CREATE TABLE IF NOT EXISTS aix_devices (
  id TEXT PRIMARY KEY,
  entity_id TEXT NOT NULL,
  client_device_id TEXT NOT NULL,
  hostname TEXT,
  platform TEXT,
  arch TEXT,
  os_version TEXT,
  metadata_json TEXT,
  first_seen_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  UNIQUE(entity_id, client_device_id)
);
CREATE INDEX IF NOT EXISTS idx_aix_devices_entity_last_seen ON aix_devices(entity_id, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS aix_sources (
  id TEXT PRIMARY KEY,
  entity_id TEXT NOT NULL,
  install_id TEXT NOT NULL UNIQUE,
  current_device_id TEXT,
  label TEXT,
  status TEXT NOT NULL,
  default_mode TEXT NOT NULL,
  expected_cadence TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  first_seen_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_success_at INTEGER,
  last_failure_at INTEGER,
  last_run_id TEXT,
  last_error TEXT,
  client_version TEXT,
  metadata_json TEXT,
  FOREIGN KEY (current_device_id) REFERENCES aix_devices(id)
);
CREATE INDEX IF NOT EXISTS idx_aix_sources_entity_created ON aix_sources(entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_aix_sources_status_last_seen ON aix_sources(status, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_aix_sources_last_run ON aix_sources(last_run_id);

CREATE TABLE IF NOT EXISTS aix_source_providers (
  source_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  enabled INTEGER NOT NULL,
  first_seen_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  stats_json TEXT,
  PRIMARY KEY (source_id, provider),
  FOREIGN KEY (source_id) REFERENCES aix_sources(id)
);

CREATE TABLE IF NOT EXISTS aix_runs (
  id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  client_run_id TEXT NOT NULL,
  trigger_kind TEXT NOT NULL,
  run_mode TEXT NOT NULL,
  status TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  completed_at INTEGER,
  sessions_seen INTEGER NOT NULL DEFAULT 0,
  sessions_changed INTEGER NOT NULL DEFAULT 0,
  imported INTEGER NOT NULL DEFAULT 0,
  upserted INTEGER NOT NULL DEFAULT 0,
  skipped INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  bytes_uploaded INTEGER NOT NULL DEFAULT 0,
  upload_count INTEGER NOT NULL DEFAULT 0,
  error_code TEXT,
  error_message TEXT,
  checkpoint_json TEXT,
  metadata_json TEXT,
  UNIQUE(source_id, client_run_id),
  FOREIGN KEY (source_id) REFERENCES aix_sources(id)
);
CREATE INDEX IF NOT EXISTS idx_aix_runs_entity_started ON aix_runs(entity_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_aix_runs_source_started ON aix_runs(source_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_aix_runs_status_started ON aix_runs(status, started_at DESC);

CREATE TABLE IF NOT EXISTS aix_uploads (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  status TEXT NOT NULL,
  content_kind TEXT NOT NULL,
  payload_sha256 TEXT NOT NULL,
  chunk_total INTEGER NOT NULL,
  received_ranges_json TEXT NOT NULL,
  bytes_received INTEGER NOT NULL DEFAULT 0,
  item_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  completed_at INTEGER,
  error_code TEXT,
  error_message TEXT,
  result_json TEXT,
  FOREIGN KEY (run_id) REFERENCES aix_runs(id),
  FOREIGN KEY (source_id) REFERENCES aix_sources(id)
);
CREATE INDEX IF NOT EXISTS idx_aix_uploads_run_created ON aix_uploads(run_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_aix_uploads_status_updated ON aix_uploads(status, updated_at DESC);
`;

function parseJson<T>(value: unknown): T | null {
  if (typeof value !== "string" || value.trim().length === 0) {
    return null;
  }
  try {
    return JSON.parse(value) as T;
  } catch {
    return null;
  }
}

function stringifyJson(value: unknown): string | null {
  if (value === undefined) {
    return null;
  }
  return value === null ? null : JSON.stringify(value);
}

function asNullableString(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

function asNullableNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function mapCredentialRow(row: Record<string, unknown>): AixCredentialRecord {
  return {
    id: String(row.id ?? ""),
    tokenId: String(row.token_id ?? ""),
    entityId: String(row.entity_id ?? ""),
    purpose: String(row.purpose ?? ""),
    label: asNullableString(row.label),
    preset: String(row.preset ?? "offboarding") as AixPreset,
    issuedByEntityId: String(row.issued_by_entity_id ?? ""),
    createdAt: Number(row.created_at ?? 0),
    firstUsedAt: asNullableNumber(row.first_used_at),
    lastUsedAt: asNullableNumber(row.last_used_at),
    expiresAt: asNullableNumber(row.expires_at),
    revokedAt: asNullableNumber(row.revoked_at),
    metadata: parseJson<Record<string, unknown>>(row.metadata_json),
  };
}

function mapDeviceRow(row: Record<string, unknown>): AixDeviceRecord {
  return {
    id: String(row.id ?? ""),
    entityId: String(row.entity_id ?? ""),
    clientDeviceId: String(row.client_device_id ?? ""),
    hostname: asNullableString(row.hostname),
    platform: asNullableString(row.platform),
    arch: asNullableString(row.arch),
    osVersion: asNullableString(row.os_version),
    metadata: parseJson<Record<string, unknown>>(row.metadata_json),
    firstSeenAt: Number(row.first_seen_at ?? 0),
    lastSeenAt: Number(row.last_seen_at ?? 0),
  };
}

function mapSourceRow(row: Record<string, unknown>): AixSourceRecord {
  return {
    id: String(row.id ?? ""),
    entityId: String(row.entity_id ?? ""),
    installId: String(row.install_id ?? ""),
    currentDeviceId: asNullableString(row.current_device_id),
    label: asNullableString(row.label),
    status: String(row.status ?? "active") as AixSourceStatus,
    defaultMode: String(row.default_mode ?? "backfill") as AixMode,
    expectedCadence: String(row.expected_cadence ?? "manual") as AixCadence,
    createdAt: Number(row.created_at ?? 0),
    firstSeenAt: Number(row.first_seen_at ?? 0),
    lastSeenAt: Number(row.last_seen_at ?? 0),
    lastSuccessAt: asNullableNumber(row.last_success_at),
    lastFailureAt: asNullableNumber(row.last_failure_at),
    lastRunId: asNullableString(row.last_run_id),
    lastError: asNullableString(row.last_error),
    clientVersion: asNullableString(row.client_version),
    metadata: parseJson<Record<string, unknown>>(row.metadata_json),
  };
}

function mapSourceProviderRow(row: Record<string, unknown>): AixSourceProviderRecord {
  return {
    sourceId: String(row.source_id ?? ""),
    provider: String(row.provider ?? ""),
    enabled: Number(row.enabled ?? 0) === 1,
    firstSeenAt: Number(row.first_seen_at ?? 0),
    lastSeenAt: Number(row.last_seen_at ?? 0),
    stats: parseJson<Record<string, unknown>>(row.stats_json),
  };
}

function mapRunRow(row: Record<string, unknown>): AixRunRecord {
  return {
    id: String(row.id ?? ""),
    sourceId: String(row.source_id ?? ""),
    entityId: String(row.entity_id ?? ""),
    clientRunId: String(row.client_run_id ?? ""),
    triggerKind: String(row.trigger_kind ?? "manual") as AixTriggerKind,
    runMode: String(row.run_mode ?? "backfill") as AixMode,
    status: String(row.status ?? "running") as AixRunStatus,
    startedAt: Number(row.started_at ?? 0),
    completedAt: asNullableNumber(row.completed_at),
    sessionsSeen: Number(row.sessions_seen ?? 0),
    sessionsChanged: Number(row.sessions_changed ?? 0),
    imported: Number(row.imported ?? 0),
    upserted: Number(row.upserted ?? 0),
    skipped: Number(row.skipped ?? 0),
    failed: Number(row.failed ?? 0),
    bytesUploaded: Number(row.bytes_uploaded ?? 0),
    uploadCount: Number(row.upload_count ?? 0),
    errorCode: asNullableString(row.error_code),
    errorMessage: asNullableString(row.error_message),
    checkpoint: parseJson<Record<string, unknown>>(row.checkpoint_json),
    metadata: parseJson<Record<string, unknown>>(row.metadata_json),
  };
}

function mapUploadRow(row: Record<string, unknown>): AixUploadRecord {
  return {
    id: String(row.id ?? ""),
    runId: String(row.run_id ?? ""),
    sourceId: String(row.source_id ?? ""),
    status: String(row.status ?? "staging") as AixUploadStatus,
    contentKind: String(row.content_kind ?? ""),
    payloadSha256: String(row.payload_sha256 ?? ""),
    chunkTotal: Number(row.chunk_total ?? 0),
    receivedRanges: parseJson<AixUploadRange[]>(row.received_ranges_json) ?? [],
    bytesReceived: Number(row.bytes_received ?? 0),
    itemCount: Number(row.item_count ?? 0),
    createdAt: Number(row.created_at ?? 0),
    updatedAt: Number(row.updated_at ?? 0),
    completedAt: asNullableNumber(row.completed_at),
    errorCode: asNullableString(row.error_code),
    errorMessage: asNullableString(row.error_message),
    result: parseJson<Record<string, unknown>>(row.result_json),
  };
}

function normalizeUploadRanges(indexes: number[]): AixUploadRange[] {
  const uniqueSorted = [...new Set(indexes.filter((value) => Number.isInteger(value) && value >= 0))].sort(
    (a, b) => a - b,
  );
  const ranges: AixUploadRange[] = [];
  for (const index of uniqueSorted) {
    const last = ranges[ranges.length - 1];
    if (!last || index > last.end + 1) {
      ranges.push({ start: index, end: index });
      continue;
    }
    last.end = index;
  }
  return ranges;
}

export function listReceivedChunkIndexes(spoolDir: string): number[] {
  if (!fs.existsSync(spoolDir)) {
    return [];
  }
  const entries = fs.readdirSync(spoolDir, { withFileTypes: true });
  return entries
    .filter((entry) => entry.isFile() && /^chunk-\d{6}$/.test(entry.name))
    .map((entry) => Number.parseInt(entry.name.slice("chunk-".length), 10))
    .filter((value) => Number.isInteger(value) && value >= 0)
    .sort((a, b) => a - b);
}

export function openAixControlDb(dataDir: string): DatabaseSync {
  fs.mkdirSync(dataDir, { recursive: true });
  const dbPath = path.join(dataDir, AIX_CONTROL_DB_NAME);
  const db = new DatabaseSync(dbPath);
  db.exec(AIX_CONTROL_SCHEMA_SQL);
  return db;
}

export function listCredentials(
  db: DatabaseSync,
  params: {
    entityId?: string;
    includeRevoked?: boolean;
    includeExpired?: boolean;
    limit?: number;
  },
): AixCredentialRecord[] {
  const clauses = ["1 = 1"];
  const values: unknown[] = [];
  if (params.entityId) {
    clauses.push("entity_id = ?");
    values.push(params.entityId);
  }
  if (!params.includeRevoked) {
    clauses.push("revoked_at IS NULL");
  }
  if (!params.includeExpired) {
    clauses.push("(expires_at IS NULL OR expires_at > ?)");
    values.push(Date.now());
  }
  values.push(Math.max(1, Math.min(500, params.limit ?? 100)));
  const rows = db
    .prepare(
      `SELECT *
       FROM aix_credentials
       WHERE ${clauses.join(" AND ")}
       ORDER BY created_at DESC
       LIMIT ?`,
    )
    .all(...values) as Record<string, unknown>[];
  return rows.map(mapCredentialRow);
}

export function findCredentialById(db: DatabaseSync, id: string): AixCredentialRecord | null {
  const row = db.prepare("SELECT * FROM aix_credentials WHERE id = ? LIMIT 1").get(id) as Record<string, unknown> | undefined;
  return row ? mapCredentialRow(row) : null;
}

export function insertCredential(
  db: DatabaseSync,
  record: Omit<AixCredentialRecord, "id"> & { id?: string },
): AixCredentialRecord {
  const id = record.id ?? randomUUID();
  db.prepare(
    `INSERT INTO aix_credentials (
       id, token_id, entity_id, purpose, label, preset, issued_by_entity_id,
       created_at, first_used_at, last_used_at, expires_at, revoked_at, metadata_json
     ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    id,
    record.tokenId,
    record.entityId,
    record.purpose,
    record.label,
    record.preset,
    record.issuedByEntityId,
    record.createdAt,
    record.firstUsedAt,
    record.lastUsedAt,
    record.expiresAt,
    record.revokedAt,
    stringifyJson(record.metadata),
  );
  return findCredentialById(db, id)!;
}

export function updateCredentialRevocation(
  db: DatabaseSync,
  params: { id: string; revokedAt: number | null },
): AixCredentialRecord | null {
  db.prepare("UPDATE aix_credentials SET revoked_at = ? WHERE id = ?").run(params.revokedAt, params.id);
  return findCredentialById(db, params.id);
}

export function createOrUpdateDevice(
  db: DatabaseSync,
  params: {
    entityId: string;
    clientDeviceId: string;
    hostname?: string | null;
    platform?: string | null;
    arch?: string | null;
    osVersion?: string | null;
    metadata?: Record<string, unknown> | null;
    now: number;
  },
): AixDeviceRecord {
  const existingRow = db
    .prepare("SELECT * FROM aix_devices WHERE entity_id = ? AND client_device_id = ? LIMIT 1")
    .get(params.entityId, params.clientDeviceId) as Record<string, unknown> | undefined;
  if (existingRow) {
    db.prepare(
      `UPDATE aix_devices
       SET hostname = ?, platform = ?, arch = ?, os_version = ?, metadata_json = ?, last_seen_at = ?
       WHERE id = ?`,
    ).run(
      params.hostname ?? null,
      params.platform ?? null,
      params.arch ?? null,
      params.osVersion ?? null,
      stringifyJson(params.metadata ?? null),
      params.now,
      existingRow.id,
    );
    const updated = db.prepare("SELECT * FROM aix_devices WHERE id = ? LIMIT 1").get(existingRow.id) as Record<string, unknown>;
    return mapDeviceRow(updated);
  }

  const id = randomUUID();
  db.prepare(
    `INSERT INTO aix_devices (
       id, entity_id, client_device_id, hostname, platform, arch, os_version,
       metadata_json, first_seen_at, last_seen_at
     ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    id,
    params.entityId,
    params.clientDeviceId,
    params.hostname ?? null,
    params.platform ?? null,
    params.arch ?? null,
    params.osVersion ?? null,
    stringifyJson(params.metadata ?? null),
    params.now,
    params.now,
  );
  return mapDeviceRow(db.prepare("SELECT * FROM aix_devices WHERE id = ? LIMIT 1").get(id) as Record<string, unknown>);
}

export function findSourceByInstallId(db: DatabaseSync, installId: string): AixSourceRecord | null {
  const row = db.prepare("SELECT * FROM aix_sources WHERE install_id = ? LIMIT 1").get(installId) as Record<string, unknown> | undefined;
  return row ? mapSourceRow(row) : null;
}

export function findSourceById(db: DatabaseSync, sourceId: string): AixSourceRecord | null {
  const row = db.prepare("SELECT * FROM aix_sources WHERE id = ? LIMIT 1").get(sourceId) as Record<string, unknown> | undefined;
  return row ? mapSourceRow(row) : null;
}

export function upsertSource(
  db: DatabaseSync,
  params: {
    entityId: string;
    installId: string;
    currentDeviceId: string | null;
    label: string | null;
    status: AixSourceStatus;
    defaultMode: AixMode;
    expectedCadence: AixCadence;
    clientVersion: string | null;
    metadata?: Record<string, unknown> | null;
    now: number;
  },
): AixSourceRecord {
  const existing = findSourceByInstallId(db, params.installId);
  if (existing) {
    db.prepare(
      `UPDATE aix_sources
       SET current_device_id = ?, label = COALESCE(?, label), status = ?, default_mode = ?,
           expected_cadence = ?, last_seen_at = ?, client_version = ?, metadata_json = ?
       WHERE id = ?`,
    ).run(
      params.currentDeviceId,
      params.label,
      params.status,
      params.defaultMode,
      params.expectedCadence,
      params.now,
      params.clientVersion,
      stringifyJson(params.metadata ?? existing.metadata),
      existing.id,
    );
    return findSourceById(db, existing.id)!;
  }

  const id = randomUUID();
  db.prepare(
    `INSERT INTO aix_sources (
       id, entity_id, install_id, current_device_id, label, status, default_mode,
       expected_cadence, created_at, first_seen_at, last_seen_at, client_version, metadata_json
     ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    id,
    params.entityId,
    params.installId,
    params.currentDeviceId,
    params.label,
    params.status,
    params.defaultMode,
    params.expectedCadence,
    params.now,
    params.now,
    params.now,
    params.clientVersion,
    stringifyJson(params.metadata ?? null),
  );
  return findSourceById(db, id)!;
}

export function upsertSourceProviders(
  db: DatabaseSync,
  params: {
    sourceId: string;
    providers: Array<{ provider: string; enabled: boolean; stats?: Record<string, unknown> | null }>;
    now: number;
  },
): AixSourceProviderRecord[] {
  for (const provider of params.providers) {
    const name = provider.provider.trim();
    if (!name) {
      continue;
    }
    const existing = db
      .prepare("SELECT * FROM aix_source_providers WHERE source_id = ? AND provider = ? LIMIT 1")
      .get(params.sourceId, name) as Record<string, unknown> | undefined;
    if (existing) {
      db.prepare(
        `UPDATE aix_source_providers
         SET enabled = ?, last_seen_at = ?, stats_json = ?
         WHERE source_id = ? AND provider = ?`,
      ).run(provider.enabled ? 1 : 0, params.now, stringifyJson(provider.stats ?? null), params.sourceId, name);
      continue;
    }
    db.prepare(
      `INSERT INTO aix_source_providers (
         source_id, provider, enabled, first_seen_at, last_seen_at, stats_json
       ) VALUES (?, ?, ?, ?, ?, ?)`,
    ).run(
      params.sourceId,
      name,
      provider.enabled ? 1 : 0,
      params.now,
      params.now,
      stringifyJson(provider.stats ?? null),
    );
  }
  const rows = db
    .prepare("SELECT * FROM aix_source_providers WHERE source_id = ? ORDER BY provider ASC")
    .all(params.sourceId) as Record<string, unknown>[];
  return rows.map(mapSourceProviderRow);
}

export function listSourceProviders(db: DatabaseSync, sourceId: string): AixSourceProviderRecord[] {
  const rows = db
    .prepare("SELECT * FROM aix_source_providers WHERE source_id = ? ORDER BY provider ASC")
    .all(sourceId) as Record<string, unknown>[];
  return rows.map(mapSourceProviderRow);
}

export function listSources(
  db: DatabaseSync,
  params: { entityId?: string; status?: AixSourceStatus; provider?: string; limit?: number },
): Array<AixSourceRecord & { providers: AixSourceProviderRecord[]; health: AixSourceHealth }> {
  const clauses = ["1 = 1"];
  const values: unknown[] = [];
  if (params.entityId) {
    clauses.push("s.entity_id = ?");
    values.push(params.entityId);
  }
  if (params.status) {
    clauses.push("s.status = ?");
    values.push(params.status);
  }
  if (params.provider) {
    clauses.push(
      "EXISTS (SELECT 1 FROM aix_source_providers sp WHERE sp.source_id = s.id AND sp.provider = ?)",
    );
    values.push(params.provider);
  }
  values.push(Math.max(1, Math.min(500, params.limit ?? 100)));
  const rows = db
    .prepare(
      `SELECT s.*
       FROM aix_sources s
       WHERE ${clauses.join(" AND ")}
       ORDER BY s.last_seen_at DESC, s.created_at DESC
       LIMIT ?`,
    )
    .all(...values) as Record<string, unknown>[];
  return rows.map((row) => {
    const source = mapSourceRow(row);
    const providers = listSourceProviders(db, source.id);
    return {
      ...source,
      providers,
      health: computeSourceHealth(source),
    };
  });
}

export function updateSource(
  db: DatabaseSync,
  params: {
    sourceId: string;
    entityId?: string;
    label?: string | null;
    status?: AixSourceStatus;
    defaultMode?: AixMode;
    expectedCadence?: AixCadence;
  },
): AixSourceRecord | null {
  const existing = findSourceById(db, params.sourceId);
  if (!existing) {
    return null;
  }
  if (params.entityId && existing.entityId !== params.entityId) {
    return null;
  }
  db.prepare(
    `UPDATE aix_sources
     SET label = ?, status = ?, default_mode = ?, expected_cadence = ?
     WHERE id = ?`,
  ).run(
    params.label !== undefined ? params.label : existing.label,
    params.status ?? existing.status,
    params.defaultMode ?? existing.defaultMode,
    params.expectedCadence ?? existing.expectedCadence,
    params.sourceId,
  );
  return findSourceById(db, params.sourceId);
}

export function beginRun(
  db: DatabaseSync,
  params: {
    source: AixSourceRecord;
    clientRunId: string;
    triggerKind: AixTriggerKind;
    runMode: AixMode;
    clientVersion: string;
    providerSummary?: Array<{ provider: string; sessionsSeen: number; sessionsChanged: number }>;
    checkpoint?: Record<string, unknown> | null;
    now: number;
  },
): { run: AixRunRecord; existing: boolean } {
  const existing = db
    .prepare("SELECT * FROM aix_runs WHERE source_id = ? AND client_run_id = ? LIMIT 1")
    .get(params.source.id, params.clientRunId) as Record<string, unknown> | undefined;
  if (existing) {
    db.prepare(
      `UPDATE aix_runs
       SET status = 'running', trigger_kind = ?, run_mode = ?, checkpoint_json = ?, metadata_json = ?
       WHERE id = ?`,
    ).run(
      params.triggerKind,
      params.runMode,
      stringifyJson(params.checkpoint ?? null),
      stringifyJson({ providerSummary: params.providerSummary ?? [] }),
      existing.id,
    );
    db.prepare(
      `UPDATE aix_sources
       SET last_seen_at = ?, client_version = ?
       WHERE id = ?`,
    ).run(params.now, params.clientVersion, params.source.id);
    return {
      run: mapRunRow(db.prepare("SELECT * FROM aix_runs WHERE id = ? LIMIT 1").get(existing.id) as Record<string, unknown>),
      existing: true,
    };
  }

  const id = randomUUID();
  db.prepare(
    `INSERT INTO aix_runs (
       id, source_id, entity_id, client_run_id, trigger_kind, run_mode, status,
       started_at, checkpoint_json, metadata_json
     ) VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?, ?)`,
  ).run(
    id,
    params.source.id,
    params.source.entityId,
    params.clientRunId,
    params.triggerKind,
    params.runMode,
    params.now,
    stringifyJson(params.checkpoint ?? null),
    stringifyJson({ providerSummary: params.providerSummary ?? [] }),
  );
  db.prepare(
    `UPDATE aix_sources
     SET last_seen_at = ?, client_version = ?
     WHERE id = ?`,
  ).run(params.now, params.clientVersion, params.source.id);
  return {
    run: mapRunRow(db.prepare("SELECT * FROM aix_runs WHERE id = ? LIMIT 1").get(id) as Record<string, unknown>),
    existing: false,
  };
}

export function completeRun(
  db: DatabaseSync,
  params: {
    source: AixSourceRecord;
    runId: string;
    status: AixRunStatus;
    sessionsSeen: number;
    sessionsChanged: number;
    imported: number;
    upserted: number;
    skipped: number;
    failed: number;
    bytesUploaded: number;
    uploadCount: number;
    checkpoint?: Record<string, unknown> | null;
    errorCode?: string | null;
    errorMessage?: string | null;
    now: number;
  },
): AixRunRecord | null {
  const existing = db
    .prepare("SELECT * FROM aix_runs WHERE id = ? AND source_id = ? LIMIT 1")
    .get(params.runId, params.source.id) as Record<string, unknown> | undefined;
  if (!existing) {
    return null;
  }
  db.prepare(
    `UPDATE aix_runs
     SET status = ?, completed_at = ?, sessions_seen = ?, sessions_changed = ?,
         imported = ?, upserted = ?, skipped = ?, failed = ?, bytes_uploaded = ?,
         upload_count = ?, checkpoint_json = ?, error_code = ?, error_message = ?
     WHERE id = ?`,
  ).run(
    params.status,
    params.now,
    Math.max(0, params.sessionsSeen),
    Math.max(0, params.sessionsChanged),
    Math.max(0, params.imported),
    Math.max(0, params.upserted),
    Math.max(0, params.skipped),
    Math.max(0, params.failed),
    Math.max(0, params.bytesUploaded),
    Math.max(0, params.uploadCount),
    stringifyJson(params.checkpoint ?? null),
    params.errorCode ?? null,
    params.errorMessage ?? null,
    params.runId,
  );

  if (params.status === "completed" || params.status === "completed_empty") {
    db.prepare(
      `UPDATE aix_sources
       SET last_seen_at = ?, last_success_at = ?, last_run_id = ?, last_error = NULL
       WHERE id = ?`,
    ).run(params.now, params.now, params.runId, params.source.id);
  } else {
    db.prepare(
      `UPDATE aix_sources
       SET last_seen_at = ?, last_failure_at = ?, last_run_id = ?, last_error = ?, status = ?
       WHERE id = ?`,
    ).run(
      params.now,
      params.now,
      params.runId,
      params.errorMessage ?? params.errorCode ?? "run failed",
      params.status === "failed" ? "error" : params.source.status,
      params.source.id,
    );
  }

  const row = db.prepare("SELECT * FROM aix_runs WHERE id = ? LIMIT 1").get(params.runId) as Record<string, unknown> | undefined;
  return row ? mapRunRow(row) : null;
}

export function listRuns(
  db: DatabaseSync,
  params: { entityId?: string; sourceId?: string; status?: AixRunStatus; limit?: number },
): AixRunRecord[] {
  const clauses = ["1 = 1"];
  const values: unknown[] = [];
  if (params.entityId) {
    clauses.push("entity_id = ?");
    values.push(params.entityId);
  }
  if (params.sourceId) {
    clauses.push("source_id = ?");
    values.push(params.sourceId);
  }
  if (params.status) {
    clauses.push("status = ?");
    values.push(params.status);
  }
  values.push(Math.max(1, Math.min(500, params.limit ?? 100)));
  const rows = db
    .prepare(
      `SELECT *
       FROM aix_runs
       WHERE ${clauses.join(" AND ")}
       ORDER BY started_at DESC
       LIMIT ?`,
    )
    .all(...values) as Record<string, unknown>[];
  return rows.map(mapRunRow);
}

export function findRunById(db: DatabaseSync, runId: string): AixRunRecord | null {
  const row = db.prepare("SELECT * FROM aix_runs WHERE id = ? LIMIT 1").get(runId) as Record<string, unknown> | undefined;
  return row ? mapRunRow(row) : null;
}

export function listUploadsForRun(
  db: DatabaseSync,
  runId: string,
): Array<{
  id: string;
  status: string;
  payloadSha256: string;
  chunkTotal: number;
  bytesReceived: number;
  itemCount: number;
  createdAt: number;
  completedAt: number | null;
}> {
  const rows = db
    .prepare(
      `SELECT id, status, payload_sha256, chunk_total, bytes_received, item_count, created_at, completed_at
       FROM aix_uploads
       WHERE run_id = ?
       ORDER BY created_at ASC`,
    )
    .all(runId) as Record<string, unknown>[];
  return rows.map((row) => ({
    id: String(row.id ?? ""),
    status: String(row.status ?? ""),
    payloadSha256: String(row.payload_sha256 ?? ""),
    chunkTotal: Number(row.chunk_total ?? 0),
    bytesReceived: Number(row.bytes_received ?? 0),
    itemCount: Number(row.item_count ?? 0),
    createdAt: Number(row.created_at ?? 0),
    completedAt: asNullableNumber(row.completed_at),
  }));
}

export function findUploadById(db: DatabaseSync, uploadId: string): AixUploadRecord | null {
  const row = db
    .prepare("SELECT * FROM aix_uploads WHERE id = ? LIMIT 1")
    .get(uploadId) as Record<string, unknown> | undefined;
  return row ? mapUploadRow(row) : null;
}

export function beginUpload(
  db: DatabaseSync,
  params: {
    id: string;
    runId: string;
    sourceId: string;
    contentKind: string;
    payloadSha256: string;
    chunkTotal: number;
    bytesTotal?: number;
    itemCount: number;
    now: number;
  },
): AixUploadRecord {
  const existing = findUploadById(db, params.id);
  if (existing) {
    db.prepare(
      `UPDATE aix_uploads
       SET status = 'staging', updated_at = ?, completed_at = NULL, item_count = ?, chunk_total = ?,
           payload_sha256 = ?, content_kind = ?, error_code = NULL, error_message = NULL, result_json = NULL
       WHERE id = ?`,
    ).run(
      params.now,
      params.itemCount,
      params.chunkTotal,
      params.payloadSha256,
      params.contentKind,
      params.id,
    );
    return findUploadById(db, params.id)!;
  }
  db.prepare(
    `INSERT INTO aix_uploads (
       id, run_id, source_id, status, content_kind, payload_sha256, chunk_total,
       received_ranges_json, bytes_received, item_count, created_at, updated_at
     ) VALUES (?, ?, ?, 'staging', ?, ?, ?, '[]', 0, ?, ?, ?)`,
  ).run(
    params.id,
    params.runId,
    params.sourceId,
    params.contentKind,
    params.payloadSha256,
    params.chunkTotal,
    params.itemCount,
    params.now,
    params.now,
  );
  return findUploadById(db, params.id)!;
}

export function updateUploadProgress(
  db: DatabaseSync,
  params: {
    uploadId: string;
    bytesReceived: number;
    receivedChunkIndexes: number[];
    now: number;
  },
): AixUploadRecord | null {
  db.prepare(
    `UPDATE aix_uploads
     SET bytes_received = ?, received_ranges_json = ?, updated_at = ?
     WHERE id = ?`,
  ).run(
    Math.max(0, params.bytesReceived),
    JSON.stringify(normalizeUploadRanges(params.receivedChunkIndexes)),
    params.now,
    params.uploadId,
  );
  return findUploadById(db, params.uploadId);
}

export function completeUpload(
  db: DatabaseSync,
  params: {
    uploadId: string;
    status: AixUploadStatus;
    bytesReceived: number;
    receivedChunkIndexes: number[];
    result?: Record<string, unknown> | null;
    errorCode?: string | null;
    errorMessage?: string | null;
    now: number;
  },
): AixUploadRecord | null {
  db.prepare(
    `UPDATE aix_uploads
     SET status = ?, bytes_received = ?, received_ranges_json = ?, updated_at = ?, completed_at = ?,
         error_code = ?, error_message = ?, result_json = ?
     WHERE id = ?`,
  ).run(
    params.status,
    Math.max(0, params.bytesReceived),
    JSON.stringify(normalizeUploadRanges(params.receivedChunkIndexes)),
    params.now,
    params.status === "completed" ? params.now : null,
    params.errorCode ?? null,
    params.errorMessage ?? null,
    stringifyJson(params.result ?? null),
    params.uploadId,
  );
  return findUploadById(db, params.uploadId);
}

export function computeSourceHealth(source: AixSourceRecord): AixSourceHealth {
  if (source.status === "error") {
    return "failed";
  }
  if (source.lastFailureAt && (!source.lastSuccessAt || source.lastFailureAt >= source.lastSuccessAt)) {
    return "failed";
  }
  if (!source.lastSeenAt) {
    return "idle";
  }
  const now = Date.now();
  const staleAfterMs =
    source.expectedCadence === "live"
      ? 15 * 60 * 1000
      : source.expectedCadence === "five_minutes"
        ? 20 * 60 * 1000
        : source.expectedCadence === "ten_minutes"
          ? 40 * 60 * 1000
          : source.expectedCadence === "daily"
            ? 36 * 60 * 60 * 1000
            : Number.POSITIVE_INFINITY;
  if (Number.isFinite(staleAfterMs) && source.lastSeenAt < now - staleAfterMs) {
    return "stale";
  }
  if (!source.lastSuccessAt) {
    return "idle";
  }
  return "healthy";
}
