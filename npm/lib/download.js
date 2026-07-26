import { createWriteStream } from "node:fs";
import { chmod, mkdir, rename, unlink } from "node:fs/promises";
import { pipeline } from "node:stream/promises";
import { Readable } from "node:stream";
import path from "node:path";
import {
  binaryInstallDir,
  binaryPath,
  packageVersion,
  releaseAsset,
  releaseDownloadUrl,
  releaseTag,
} from "./paths.js";

/**
 * Ensure the langer binary for this host is present under ~/.langer/bin.
 * Returns the absolute path to the executable.
 */
export async function ensureBinary(options = {}) {
  const home = options.home;
  const version = options.version ?? packageVersion();
  const tag = releaseTag(version);
  const dest = binaryPath(home);
  const { asset } = releaseAsset(options.platform, options.arch);
  const url = options.url ?? releaseDownloadUrl(tag, asset, options.repo);

  if (options.binary) {
    return path.resolve(options.binary);
  }

  if (!options.force) {
    try {
      const { access } = await import("node:fs/promises");
      await access(dest);
      return dest;
    } catch {
      // download below
    }
  }

  if (options.dryRun) {
    return dest;
  }

  await mkdir(binaryInstallDir(home), { recursive: true });
  const tmp = `${dest}.download`;
  try {
    await downloadFile(url, tmp);
    await rename(tmp, dest);
    if (process.platform !== "win32") {
      await chmod(dest, 0o755);
    }
  } catch (err) {
    try {
      await unlink(tmp);
    } catch {
      // ignore
    }
    const message = err instanceof Error ? err.message : String(err);
    throw new Error(
      `failed to download langer ${tag} (${asset}) from ${url}: ${message}\n` +
        `Build from source instead: go install github.com/fingerskier/langer/cmd/langer@${tag}`,
    );
  }
  return dest;
}

async function downloadFile(url, dest) {
  const response = await fetch(url, {
    headers: { "User-Agent": "fingerskier-langer-install" },
    redirect: "follow",
  });
  if (!response.ok || !response.body) {
    throw new Error(`HTTP ${response.status} ${response.statusText}`);
  }
  await pipeline(Readable.fromWeb(response.body), createWriteStream(dest));
}
