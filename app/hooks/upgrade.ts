import fs from "node:fs";
import path from "node:path";
import type { NexAppHookContext } from "../../../../../nex/src/apps/context.js";
import { openAixControlDb } from "../methods/store.js";

export default async function onUpgrade(ctx: NexAppHookContext): Promise<void> {
  fs.mkdirSync(path.join(ctx.app.dataDir, "spool", "uploads"), { recursive: true });
  const db = openAixControlDb(ctx.app.dataDir);
  db.close();
}
