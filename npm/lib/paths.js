import { homedir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);

export function packageRoot() {
  return path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
}

export function packageVersion() {
  const pkg = require(path.join(packageRoot(), "package.json"));
  return String(pkg.version);
}

export function releaseTag(version = packageVersion()) {
  return version.startsWith("v") ? version : `v${version}`;
}

/** Stable install location for downloaded binaries. */
export function binaryInstallDir(home = homedir()) {
  return path.join(home, ".langer", "bin");
}

export function binaryName(platform = process.platform) {
  return platform === "win32" ? "langer.exe" : "langer";
}

export function binaryPath(home = homedir(), platform = process.platform) {
  return path.join(binaryInstallDir(home), binaryName(platform));
}

/**
 * Asset name uploaded by .github/workflows/release.yml for this host.
 * @returns {{ asset: string, goos: string, goarch: string }}
 */
export function releaseAsset(platform = process.platform, arch = process.arch) {
  const goarch = arch === "arm64" ? "arm64" : arch === "x64" || arch === "amd64" ? "amd64" : null;
  if (!goarch) {
    throw new Error(`unsupported CPU architecture: ${arch}`);
  }
  let goos;
  if (platform === "win32") goos = "windows";
  else if (platform === "darwin") goos = "darwin";
  else if (platform === "linux") goos = "linux";
  else throw new Error(`unsupported platform: ${platform}`);

  const ext = goos === "windows" ? ".exe" : "";
  return {
    goos,
    goarch,
    asset: `langer_${goos}_${goarch}${ext}`,
  };
}

export function releaseDownloadUrl(tag, asset, repo = "fingerskier/langer") {
  return `https://github.com/${repo}/releases/download/${tag}/${asset}`;
}

export function toForwardSlashes(p) {
  return p.replace(/\\/g, "/");
}
