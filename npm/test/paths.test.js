import test from "node:test";
import assert from "node:assert/strict";
import {
  binaryName,
  releaseAsset,
  releaseDownloadUrl,
  releaseTag,
} from "../lib/paths.js";

test("releaseTag adds v prefix", () => {
  assert.equal(releaseTag("0.1.0"), "v0.1.0");
  assert.equal(releaseTag("v0.1.0"), "v0.1.0");
});

test("releaseAsset maps node platform to go assets", () => {
  assert.deepEqual(releaseAsset("win32", "x64"), {
    goos: "windows",
    goarch: "amd64",
    asset: "langer_windows_amd64.exe",
  });
  assert.deepEqual(releaseAsset("darwin", "arm64"), {
    goos: "darwin",
    goarch: "arm64",
    asset: "langer_darwin_arm64",
  });
  assert.deepEqual(releaseAsset("linux", "x64"), {
    goos: "linux",
    goarch: "amd64",
    asset: "langer_linux_amd64",
  });
});

test("binaryName is platform-specific", () => {
  assert.equal(binaryName("win32"), "langer.exe");
  assert.equal(binaryName("linux"), "langer");
});

test("releaseDownloadUrl is stable", () => {
  assert.equal(
    releaseDownloadUrl("v0.1.0", "langer_linux_amd64"),
    "https://github.com/fingerskier/langer/releases/download/v0.1.0/langer_linux_amd64",
  );
});
