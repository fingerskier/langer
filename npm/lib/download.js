import { createWriteStream, constants as fsConstants } from "node:fs";
import {
  access,
  chmod,
  copyFile,
  mkdir,
  open,
  readFile,
  rename,
  unlink,
  writeFile,
} from "node:fs/promises";
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
 *
 * Download order: explicit --version/package version tag, then (unless the
 * user pinned --version) the latest GitHub release that publishes this asset.
 * That softens brief npm-vs-tag skew (e.g. npm 0.7.3 while GH assets are v0.7.0).
 *
 * A sibling `langer[.exe].version` stamp records the npm package version that
 * last installed the binary. If the stamp is missing or differs from the
 * current package version, the binary is re-downloaded.
 *
 * On Windows, replacing a running langer.exe uses rename-aside (running EXEs
 * can usually be renamed; they cannot always be overwritten in place).
 */
export async function ensureBinary(options = {}) {
  const home = options.home;
  const versionPinned = Boolean(options.version);
  const version = options.version ?? packageVersion();
  const tag = releaseTag(version);
  const dest = binaryPath(home);
  const stampPath = `${dest}.version`;
  const { asset } = releaseAsset(options.platform, options.arch);
  const repo = options.repo ?? "fingerskier/langer";

  if (options.binary) {
    return path.resolve(options.binary);
  }

  if (!options.force) {
    try {
      await access(dest);
      const stamp = await readStamp(stampPath);
      if (stamp === version) {
        return dest;
      }
    } catch {
      // download below
    }
  }

  if (options.dryRun) {
    return dest;
  }

  const candidates = [];
  if (options.url) {
    candidates.push({ label: options.url, url: options.url });
  } else {
    // Prefer matching tag, then latest (common when npm is ahead of GH assets).
    candidates.push({ label: tag, url: releaseDownloadUrl(tag, asset, repo) });
    if (!versionPinned) {
      candidates.push({
        label: "latest release",
        url: `https://github.com/${repo}/releases/latest/download/${asset}`,
      });
    }
  }

  await mkdir(binaryInstallDir(home), { recursive: true });
  const tmp = `${dest}.download`;
  const errors = [];
  for (const candidate of candidates) {
    try {
      await downloadFile(candidate.url, tmp);
      await installBinaryFile(tmp, dest);
      if (process.platform !== "win32") {
        await chmod(dest, 0o755);
      }
      await writeFile(stampPath, `${version}\n`, "utf8");
      return dest;
    } catch (err) {
      try {
        await unlink(tmp);
      } catch {
        // ignore
      }
      const message = err instanceof Error ? err.message : String(err);
      errors.push(`${candidate.label} (${candidate.url}): ${message}`);
    }
  }

  throw new Error(
    `failed to download langer (${asset}):\n  - ${errors.join("\n  - ")}\n` +
      `If you see EPERM/EACCES on Windows, stop agents using langer MCP (or kill langer.exe), then retry.\n` +
      `Build from source: go install github.com/fingerskier/langer/cmd/langer@${tag}\n` +
      `Or pin a release with assets: npx @fingerskier/langer ensure --version 0.7.0 --force`,
  );
}

/**
 * Move a downloaded file into place. Windows: rename existing exe aside first
 * so a running MCP binary does not block the update (EPERM on rename/overwrite).
 */
export async function installBinaryFile(tmpPath, destPath) {
  try {
    await access(destPath);
  } catch {
    await rename(tmpPath, destPath);
    return;
  }

  if (process.platform === "win32") {
    const aside = `${destPath}.old`;
    try {
      await unlink(aside);
    } catch {
      // ignore missing
    }
    try {
      // Running EXEs can usually be renamed on Windows.
      await rename(destPath, aside);
    } catch {
      // Fall through to copy/overwrite attempts.
    }
    try {
      await rename(tmpPath, destPath);
      try {
        await unlink(aside);
      } catch {
        // still running from aside path — leave for next install
      }
      return;
    } catch (err) {
      // try copy into place
      try {
        await copyFile(tmpPath, destPath);
        await unlink(tmpPath);
        try {
          await unlink(aside);
        } catch {
          // ignore
        }
        return;
      } catch {
        throw err;
      }
    }
  }

  // Unix: replace atomically when possible.
  try {
    await rename(tmpPath, destPath);
    return;
  } catch {
    await copyFile(tmpPath, destPath);
    await unlink(tmpPath);
  }
}

async function readStamp(stampPath) {
  try {
    const text = await readFile(stampPath, "utf8");
    return text.trim();
  } catch {
    return "";
  }
}

async function downloadFile(url, dest) {
  const response = await fetch(url, {
    headers: { "User-Agent": "fingerskier-langer-install" },
    redirect: "follow",
  });
  if (!response.ok || !response.body) {
    throw new Error(`HTTP ${response.status} ${response.statusText}`);
  }
  // Truncate if a previous failed download left a partial file.
  const handle = await open(dest, fsConstants.O_CREAT | fsConstants.O_TRUNC | fsConstants.O_WRONLY, 0o600);
  try {
    await pipeline(Readable.fromWeb(response.body), handle.createWriteStream());
  } finally {
    await handle.close();
  }
}
