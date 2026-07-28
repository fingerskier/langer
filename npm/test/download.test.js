import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile, access } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { installBinaryFile } from "../lib/download.js";

test("installBinaryFile replaces an existing binary", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "langer-dl-"));
  const dest = path.join(dir, "langer.exe");
  const tmp = path.join(dir, "langer.exe.download");
  await writeFile(dest, "old-binary");
  await writeFile(tmp, "new-binary");
  await installBinaryFile(tmp, dest);
  const body = await readFile(dest, "utf8");
  assert.equal(body, "new-binary");
  await assert.rejects(() => access(tmp), /ENOENT/);
});
