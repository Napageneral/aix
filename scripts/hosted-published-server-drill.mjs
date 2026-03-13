#!/usr/bin/env node

import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtemp, mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { setTimeout as sleep } from "node:timers/promises";

const AIX_ROOT = "/Users/tyler/nexus/home/projects/aix";
const FRONTDOOR_ORIGIN = text(process.env.FRONTDOOR_ORIGIN) || "https://frontdoor.nexushub.sh";
const APP_ID = "aix";
const PLAN_ID = text(process.env.FRONTDOOR_DRILL_PLAN) || "cax11";
const PUSH_SOURCE = text(process.env.AIX_DRILL_SOURCE) || "codex,claude-code,nexus";
const PUSH_MODE = "backfill";

function text(value) {
  return typeof value === "string" ? value.trim() : "";
}

function sanitizeUsername(value) {
  return text(value).toLowerCase().replace(/[^a-z0-9_-]/g, "");
}

function fail(message, details = {}) {
  const payload = {
    ok: false,
    error: message,
    ...details,
  };
  process.stderr.write(`${JSON.stringify(payload, null, 2)}\n`);
  process.exit(1);
}

function sessionCookieFromSetCookie(setCookie) {
  if (!setCookie) {
    return "";
  }
  const first = setCookie.split(",")[0] ?? "";
  return first.split(";")[0] ?? "";
}

async function requestJson(url, options = {}) {
  const response = await fetch(url, {
    cache: "no-store",
    redirect: "manual",
    ...options,
  });
  const raw = await response.text();
  let body = null;
  try {
    body = raw ? JSON.parse(raw) : null;
  } catch {
    body = null;
  }
  return { response, raw, body };
}

async function getJson(url, headers = {}) {
  return await requestJson(url, {
    method: "GET",
    headers,
  });
}

async function postJson(url, payload, headers = {}) {
  return await requestJson(url, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      ...headers,
    },
    body: JSON.stringify(payload),
  });
}

function responseSummary(result) {
  return {
    status: result.response.status,
    ok: result.response.ok,
    body: result.body ?? result.raw,
  };
}

async function execFileText(file, args, options = {}) {
  return await new Promise((resolve, reject) => {
    execFile(
      file,
      args,
      {
        ...options,
        maxBuffer: 32 * 1024 * 1024,
      },
      (error, stdout, stderr) => {
        if (error) {
          reject(
            new Error(
              [
                `${file} ${args.join(" ")} failed`,
                `stdout:\n${stdout}`,
                `stderr:\n${stderr}`,
              ].join("\n\n"),
            ),
          );
          return;
        }
        resolve({ stdout, stderr });
      },
    );
  });
}

async function deriveSignupIdentity() {
  const explicitEmail = text(process.env.FRONTDOOR_DRILL_EMAIL);
  if (explicitEmail) {
    return {
      email: explicitEmail,
      displayName: text(process.env.FRONTDOOR_DRILL_DISPLAY_NAME) || "AIX Prod Drill",
    };
  }
  let gitEmail = "";
  try {
    const result = await execFileText("git", ["config", "--global", "user.email"]);
    gitEmail = text(result.stdout);
  } catch {
    gitEmail = "";
  }
  const stamp = new Date().toISOString().replace(/[-:.TZ]/g, "").slice(0, 14);
  if (gitEmail.includes("@")) {
    const [local, domain] = gitEmail.split("@");
    return {
      email: `${local}+aix-prod-drill-${stamp}@${domain}`,
      displayName: text(process.env.FRONTDOOR_DRILL_DISPLAY_NAME) || "AIX Prod Drill",
    };
  }
  return {
    email: `aix-prod-drill-${stamp}@example.com`,
    displayName: text(process.env.FRONTDOOR_DRILL_DISPLAY_NAME) || "AIX Prod Drill",
  };
}

async function authenticate(origin) {
  const username = text(process.env.FRONTDOOR_DRILL_USERNAME);
  const password = text(process.env.FRONTDOOR_DRILL_PASSWORD) || text(process.env.NEXUS_PASSWORD);
  if (!password) {
    fail("missing_frontdoor_password", {
      required_env: ["FRONTDOOR_DRILL_PASSWORD or NEXUS_PASSWORD"],
    });
  }
  if (username) {
    const login = await postJson(
      `${origin}/api/auth/login`,
      {
        username,
        password,
      },
      {},
    );
    if (!login.response.ok || login.body?.ok !== true) {
      fail("frontdoor_login_failed", {
        login: responseSummary(login),
      });
    }
    const cookie = sessionCookieFromSetCookie(login.response.headers.get("set-cookie"));
    if (!cookie) {
      fail("frontdoor_login_missing_session_cookie", {
        login: responseSummary(login),
      });
    }
    return {
      authMode: "login",
      cookie,
      signupEmail: null,
      body: login.body,
      loginUsername: username,
    };
  }

  const signupIdentity = await deriveSignupIdentity();
  const signup = await postJson(
    `${origin}/api/auth/signup`,
    {
      email: signupIdentity.email,
      password,
      display_name: signupIdentity.displayName,
      intent_app: "",
    },
    {},
  );
  if (!signup.response.ok || signup.body?.ok !== true) {
    fail("frontdoor_signup_failed", {
      signup: responseSummary(signup),
    });
  }
  const cookie = sessionCookieFromSetCookie(signup.response.headers.get("set-cookie"));
  if (!cookie) {
    fail("frontdoor_signup_missing_session_cookie", {
      signup: responseSummary(signup),
    });
  }
  const login = await postJson(
    `${origin}/api/auth/login`,
    {
      username: signupIdentity.email,
      password,
    },
    {},
  );
  if (!login.response.ok || login.body?.ok !== true) {
    fail("frontdoor_signup_login_failed", {
      signup: responseSummary(signup),
      login: responseSummary(login),
    });
  }
  const loginCookie = sessionCookieFromSetCookie(login.response.headers.get("set-cookie"));
  return {
    authMode: "signup",
    cookie: loginCookie || cookie,
    signupEmail: signupIdentity.email,
    body: login.body,
    loginUsername: signupIdentity.email,
  };
}

async function authMe(origin, cookie) {
  const me = await getJson(`${origin}/api/auth/me`, {
    cookie,
  });
  if (!me.response.ok || me.body?.ok !== true) {
    fail("frontdoor_auth_me_failed", {
      me: responseSummary(me),
    });
  }
  return me.body;
}

async function accountCredits(origin, cookie) {
  const credits = await getJson(`${origin}/api/account/credits`, {
    cookie,
  });
  if (!credits.response.ok || credits.body?.ok !== true) {
    fail("frontdoor_account_credits_failed", {
      credits: responseSummary(credits),
    });
  }
  return credits.body;
}

async function appCatalog(origin, cookie) {
  const headers = cookie ? { cookie } : {};
  const catalog = await getJson(`${origin}/api/apps/catalog`, headers);
  if (!catalog.response.ok || catalog.body?.ok !== true) {
    fail("frontdoor_app_catalog_failed", {
      catalog: responseSummary(catalog),
    });
  }
  return catalog.body;
}

async function createServer(origin, cookie) {
  const stamp = new Date().toISOString().replace(/[-:.TZ]/g, "").slice(0, 14);
  const displayName = `AIX Prod Drill ${stamp}`;
  const created = await postJson(
    `${origin}/api/servers/create`,
    {
      plan: PLAN_ID,
      display_name: displayName,
    },
    {
      cookie,
    },
  );
  if (!created.response.ok || created.body?.ok !== true) {
    fail("frontdoor_server_create_failed", {
      create: responseSummary(created),
    });
  }
  return created.body;
}

async function getServer(origin, cookie, serverId) {
  const server = await getJson(`${origin}/api/servers/${encodeURIComponent(serverId)}`, {
    cookie,
  });
  if (!server.response.ok || server.body?.ok !== true) {
    fail("frontdoor_server_get_failed", {
      server_id: serverId,
      server: responseSummary(server),
    });
  }
  return server.body.server;
}

async function waitForServerRunning(origin, cookie, serverId, timeoutMs = 300_000) {
  const startedAt = Date.now();
  let last = null;
  while (Date.now() - startedAt < timeoutMs) {
    const server = await getServer(origin, cookie, serverId);
    last = server;
    if (text(server.status) === "running") {
      return server;
    }
    await sleep(5_000);
  }
  fail("frontdoor_server_not_running", {
    server_id: serverId,
    last_server: last,
  });
}

async function purchaseAndInstall(origin, cookie, serverId) {
  const purchase = await postJson(
    `${origin}/api/apps/${encodeURIComponent(APP_ID)}/purchase`,
    {
      server_id: serverId,
      install: true,
    },
    {
      cookie,
    },
  );
  if (!purchase.response.ok || purchase.body?.ok !== true) {
    fail("frontdoor_app_purchase_install_failed", {
      purchase: responseSummary(purchase),
    });
  }
  return purchase.body;
}

async function getInstallStatus(origin, cookie, serverId) {
  const status = await getJson(
    `${origin}/api/servers/${encodeURIComponent(serverId)}/apps/${encodeURIComponent(APP_ID)}/install-status`,
    {
      cookie,
    },
  );
  if (!status.response.ok || status.body?.ok !== true) {
    fail("frontdoor_install_status_failed", {
      install_status: responseSummary(status),
    });
  }
  return status.body;
}

async function waitForInstalled(origin, cookie, serverId, timeoutMs = 180_000) {
  const startedAt = Date.now();
  let last = null;
  while (Date.now() - startedAt < timeoutMs) {
    const status = await getInstallStatus(origin, cookie, serverId);
    last = status;
    if (text(status.install_status) === "installed") {
      return status;
    }
    if (text(status.install_status) === "failed") {
      fail("frontdoor_app_install_failed", {
        install_status: status,
      });
    }
    await sleep(4_000);
  }
  fail("frontdoor_app_install_timeout", {
    server_id: serverId,
    last_install_status: last,
  });
}

async function mintRuntimeToken(origin, cookie, serverId) {
  const token = await postJson(
    `${origin}/api/runtime/token`,
    {
      server_id: serverId,
    },
    {
      cookie,
    },
  );
  if (!token.response.ok || token.body?.ok !== true) {
    fail("frontdoor_runtime_token_failed", {
      runtime_token: responseSummary(token),
    });
  }
  const accessToken = text(token.body?.access_token);
  const httpBaseUrl = text(token.body?.runtime?.http_base_url);
  if (!accessToken || !httpBaseUrl) {
    fail("frontdoor_runtime_token_incomplete", {
      runtime_token: token.body,
    });
  }
  return {
    accessToken,
    httpBaseUrl,
    runtime: token.body.runtime,
  };
}

async function runtimeOperation(httpBaseUrl, token, method, payload) {
  const result = await postJson(
    `${httpBaseUrl.replace(/\/+$/g, "")}/operations/${method}`,
    payload,
    {
      authorization: `Bearer ${token}`,
    },
  );
  if (!result.response.ok || result.body?.ok !== true) {
    fail("runtime_operation_failed", {
      method,
      operation: responseSummary(result),
    });
  }
  return result.body.payload;
}

async function waitForRuntimePublicOrigin(runtimeBaseUrl, timeoutMs = 180_000) {
  const startedAt = Date.now();
  let lastStatus = null;
  while (Date.now() - startedAt < timeoutMs) {
    try {
      const response = await fetch(`${runtimeBaseUrl.replace(/\/+$/g, "")}/runtime/health`, {
        cache: "no-store",
        redirect: "manual",
      });
      lastStatus = response.status;
      if (response.ok) {
        await response.text();
        return;
      }
    } catch (error) {
      lastStatus = String(error);
    }
    await sleep(3_000);
  }
  fail("runtime_public_origin_unreachable", {
    runtime_base_url: runtimeBaseUrl,
    last_status: lastStatus,
  });
}

async function buildAixBinary(tempRoot) {
  const binaryPath = path.join(tempRoot, "bin", "aix");
  await mkdir(path.dirname(binaryPath), { recursive: true });
  await execFileText("go", ["build", "-o", binaryPath, "./cmd/aix"], {
    cwd: AIX_ROOT,
    env: {
      ...process.env,
      GOCACHE: path.join(tempRoot, "go-cache"),
    },
  });
  return binaryPath;
}

async function runAix(binaryPath, tempRoot, args) {
  const result = await execFileText(binaryPath, args, {
    cwd: AIX_ROOT,
    env: {
      ...process.env,
      XDG_CONFIG_HOME: path.join(tempRoot, "xdg-config"),
      XDG_DATA_HOME: path.join(tempRoot, "xdg-data"),
      GOCACHE: path.join(tempRoot, "go-cache"),
    },
  });
  try {
    return JSON.parse(result.stdout);
  } catch (error) {
    fail("aix_json_parse_failed", {
      args,
      stdout: result.stdout,
      stderr: result.stderr,
      detail: String(error),
    });
  }
}

async function assertAppShell(origin, cookie, serverId) {
  const response = await fetch(
    `${origin}/app/${encodeURIComponent(APP_ID)}/?server_id=${encodeURIComponent(serverId)}`,
    {
      headers: {
        cookie,
      },
      redirect: "manual",
    },
  );
  if (response.status !== 200) {
    fail("frontdoor_app_shell_failed", {
      status: response.status,
    });
  }
}

async function main() {
  const publicCatalog = await appCatalog(FRONTDOOR_ORIGIN, "");
  const publicAixCatalogEntry =
    Array.isArray(publicCatalog.items) &&
    publicCatalog.items.find((item) => text(item?.app_id) === APP_ID);
  if (!publicAixCatalogEntry) {
    fail("aix_missing_from_frontdoor_catalog", {
      catalog_count: Array.isArray(publicCatalog.items) ? publicCatalog.items.length : null,
    });
  }

  const auth = await authenticate(FRONTDOOR_ORIGIN);
  const me = await authMe(FRONTDOOR_ORIGIN, auth.cookie);
  const credits = await accountCredits(FRONTDOOR_ORIGIN, auth.cookie);
  const catalog = await appCatalog(FRONTDOOR_ORIGIN, auth.cookie);
  const aixCatalogEntry =
    Array.isArray(catalog.items) &&
    catalog.items.find((item) => text(item?.app_id) === APP_ID);
  if (!aixCatalogEntry) {
    fail("aix_missing_from_frontdoor_catalog", {
      catalog_count: Array.isArray(catalog.items) ? catalog.items.length : null,
    });
  }

  const created = await createServer(FRONTDOOR_ORIGIN, auth.cookie);
  const serverId = text(created.server_id);
  if (!serverId) {
    fail("frontdoor_server_create_missing_server_id", {
      created,
    });
  }
  const runningServer = await waitForServerRunning(FRONTDOOR_ORIGIN, auth.cookie, serverId);

  const purchase = await purchaseAndInstall(FRONTDOOR_ORIGIN, auth.cookie, serverId);
  const installStatus = await waitForInstalled(FRONTDOOR_ORIGIN, auth.cookie, serverId);
  await assertAppShell(FRONTDOOR_ORIGIN, auth.cookie, serverId);

  const runtimeToken = await mintRuntimeToken(FRONTDOOR_ORIGIN, auth.cookie, serverId);
  const entityId = text(auth.body?.entity_id);
  if (!entityId) {
    fail("frontdoor_login_missing_entity_id", { auth_body: auth.body });
  }

  const issued = await runtimeOperation(
    runtimeToken.httpBaseUrl,
    runtimeToken.accessToken,
    "aix.credentials.issue",
    {
      entityId,
      preset: "offboarding",
      label: `AIX prod drill ${randomUUID().slice(0, 8)}`,
      defaultMode: "backfill",
      defaultCadence: "manual",
    },
  );
  const setupBundle = issued?.setupBundle ?? {};
  const runtimeBaseUrl = text(setupBundle.runtimeBaseUrl);
  const aixClientToken = text(issued?.token);
  if (!runtimeBaseUrl || !aixClientToken) {
    fail("aix_credentials_issue_incomplete", {
      issued,
    });
  }
  if (runtimeBaseUrl !== text(runningServer.runtime_public_base_url)) {
    fail("aix_setup_bundle_runtime_url_mismatch", {
      setup_bundle_runtime_base_url: runtimeBaseUrl,
      server_runtime_public_base_url: runningServer.runtime_public_base_url,
    });
  }

  await waitForRuntimePublicOrigin(runtimeBaseUrl);

  const tempRoot = await mkdtemp(path.join(os.tmpdir(), "aix-published-server-drill-"));
  const binaryPath = await buildAixBinary(tempRoot);

  const connect = await runAix(binaryPath, tempRoot, [
    "connect",
    "--url",
    runtimeBaseUrl,
    "--token",
    aixClientToken,
    "--json",
  ]);
  if (connect?.ok !== true || text(connect?.runtime_base_url) !== runtimeBaseUrl) {
    fail("aix_connect_failed", {
      connect,
    });
  }

  const push = await runAix(binaryPath, tempRoot, [
    "push",
    "--source",
    PUSH_SOURCE,
    "--mode",
    PUSH_MODE,
    "--no-export",
    "--json",
  ]);
  if (text(push?.status) !== "completed") {
    fail("aix_push_failed", {
      push,
    });
  }
  if (!(Number(push?.imported) > 0)) {
    fail("aix_push_imported_zero_sessions", {
      push,
      source: PUSH_SOURCE,
    });
  }

  const sourceId = text(push?.source_id) || text(connect?.source_id);
  const sources = await runtimeOperation(
    runtimeToken.httpBaseUrl,
    runtimeToken.accessToken,
    "aix.sources.list",
    { entityId },
  );
  const runs = await runtimeOperation(
    runtimeToken.httpBaseUrl,
    runtimeToken.accessToken,
    "aix.runs.list",
    { entityId, limit: 20 },
  );
  const importedSessions = await runtimeOperation(
    runtimeToken.httpBaseUrl,
    runtimeToken.accessToken,
    "aix.imported-sessions.list",
    { entityId, sourceId, limit: 20 },
  );

  const summary = {
    ok: true,
    auth_mode: auth.authMode,
    signup_email: auth.signupEmail,
    login_username: auth.loginUsername,
    frontdoor_origin: FRONTDOOR_ORIGIN,
    app_id: APP_ID,
    account_id: me.account_id ?? null,
    entity_id: entityId,
    free_tier: credits.free_tier ?? null,
    server: {
      server_id: serverId,
      tenant_id: created.tenant_id ?? null,
      status: runningServer.status,
      runtime_public_base_url: runningServer.runtime_public_base_url,
    },
    purchase,
    install_status: installStatus,
    runtime_descriptor: runtimeToken.runtime,
    setup_bundle: {
      runtimeBaseUrl,
      command_count: Array.isArray(setupBundle.commands) ? setupBundle.commands.length : 0,
    },
    connect,
    push,
    verification: {
      source_count: Array.isArray(sources?.sources) ? sources.sources.length : null,
      run_count: Array.isArray(runs?.runs) ? runs.runs.length : null,
      imported_session_count: Array.isArray(importedSessions?.items) ? importedSessions.items.length : null,
    },
  };
  process.stdout.write(`${JSON.stringify(summary, null, 2)}\n`);
}

main().catch((error) => {
  fail("unexpected_error", {
    detail: error instanceof Error ? `${error.message}\n${error.stack ?? ""}`.trim() : String(error),
  });
});
