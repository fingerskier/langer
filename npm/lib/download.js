import { createWriteStream } from "node:fs";
import { access, chmod, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
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
 * That softens brief npm-vs-tag skew (e.g. npm still at 0.1.0 while GH is v0.1.1).
 *
 * A sibling `langer[.exe].version` stamp records the npm package version that
 * last installed the binary. If the stamp is missing or differs from the
 * current package version, the binary is re-downloaded (stale pre-tools
 * binaries are a common case after a major bump).
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
      // Reuse only when this package version already installed the binary.
      // Missing stamp ⇒ treat as legacy install (force refresh).
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
      await rename(tmp, dest);
      if (process.platform !== "win32") {
        await chmod(dest, 0o755);
      }
      // Stamp with the *npm package* version so a 0.7.1 package that falls
      // back to latest (v0.7.0 assets) does not re-download forever.
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
      `Build from source: go install github.com/fingerskier/langer/cmd/langer@${tag}\n` +
      `Or pin a release with assets: npx @fingerskier/langer ensure --version 0.7.0 --force`,
  );
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
  await pipeline(Readable.fromWeb(response.body), createWriteStream(dest));
}
