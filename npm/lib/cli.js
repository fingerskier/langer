import { spawnSync } from "node:child_process";
import { ensureBinary } from "./download.js";
import { installIntegration } from "./install.js";
import { binaryPath, packageVersion, releaseAsset, releaseTag } from "./paths.js";

/** Subcommands of the Go binary; npm ensures the binary then execs it. */
const BINARY_COMMANDS = new Set(["tools", "status", "daemon", "mcp"]);

/**
 * @param {string[]} args
 */
export async function runCli(args) {
  const command = args[0] ?? "help";

  if (command === "help" || command === "--help" || command === "-h") {
    console.log(helpText());
    return;
  }

  if (command === "version" || command === "--version" || command === "-V") {
    console.log(packageVersion());
    return;
  }

  if (command === "path") {
    console.log(binaryPath());
    return;
  }

  if (command === "asset") {
    const { asset, goos, goarch } = releaseAsset();
    console.log(JSON.stringify({ asset, goos, goarch, tag: releaseTag() }, null, 2));
    return;
  }

  // Installer-only "ensure" (download binary). Not "langer tools ensure …".
  if (command === "ensure") {
    const parsed = parseEnsureArgs(args.slice(1));
    const dest = await ensureBinary(parsed);
    console.log(parsed.dryRun ? `Would ensure binary at ${dest}` : `Binary ready: ${dest}`);
    return;
  }

  if (command === "install") {
    const parsed = parseInstallArgs(args.slice(1));
    const plan = await installIntegration(parsed);
    for (const line of plan.summary) {
      console.log(line);
    }
    for (const file of plan.files) {
      console.log(`- ${file}`);
    }
    return;
  }

  if (BINARY_COMMANDS.has(command)) {
    await runLangerBinary(args);
    return;
  }

  console.error(`Unknown command: ${command}`);
  console.error("");
  console.error(helpText());
  process.exitCode = 1;
}

/**
 * Ensure ~/.langer/bin/langer and run it with the given argv (stdio inherited).
 * @param {string[]} args full argv after npx package name, e.g. ["tools","ensure","typescript"]
 * @param {{ binary?: string, version?: string, force?: boolean, home?: string }} [opts]
 */
export async function runLangerBinary(args, opts = {}) {
  let dest = await ensureBinary({
    binary: opts.binary,
    version: opts.version,
    force: opts.force,
    home: opts.home,
  });

  // Stale caches: pre-v0.7 binaries only expose mcp/daemon/status. If the user
  // asked for `tools` and help omits it, force a re-download once.
  if (args[0] === "tools" && !opts.binary && !opts.force) {
    const probe = spawnSync(dest, [], { encoding: "utf8", windowsHide: true });
    const help = `${probe.stdout ?? ""}${probe.stderr ?? ""}`;
    if (!/\btools\b/.test(help)) {
      dest = await ensureBinary({
        version: opts.version,
        force: true,
        home: opts.home,
      });
    }
  }

  const result = spawnSync(dest, args, {
    stdio: "inherit",
    windowsHide: true,
  });
  if (result.error) {
    throw result.error;
  }
  if (typeof result.status === "number" && result.status !== 0) {
    process.exitCode = result.status;
  }
  if (result.signal) {
    process.exitCode = 1;
  }
}

export function helpText() {
  return [
    "langer-install — download langer binaries and wire MCP hosts",
    "",
    "USAGE",
    "  npx -y @fingerskier/langer <command> [options]",
    "",
    "INSTALLER COMMANDS",
    "  install <claude|grok|codex|all>   Download the binary (if needed) and register MCP",
    "  ensure                            Download/install the host binary under ~/.langer/bin",
    "  path                              Print the binary install path",
    "  asset                             Print the GitHub release asset name for this host",
    "  version                           Print this npm package version",
    "  help                              Show this help",
    "",
    "BINARY COMMANDS (ensures binary, then runs it)",
    "  tools list|ensure <id>|update     Managed language-server installs (~/.langer/tools)",
    "  status                            Daemon / index status for the current workspace",
    "  mcp --stdio                       MCP frontend (normally via agent MCP config)",
    "  daemon <root>                     Run the workspace daemon explicitly",
    "",
    "INSTALL OPTIONS",
    "  --scope user|repo     Default: user (home config). repo writes into the current directory.",
    "  --binary PATH         Use an existing langer binary instead of downloading.",
    "  --version X.Y.Z       GitHub release tag without or with leading v (default: package version).",
    "  --force               Re-download the binary even if ~/.langer/bin already has one.",
    "  --dry-run             Print planned writes without changing the filesystem.",
    "",
    "EXAMPLES",
    "  npx -y @fingerskier/langer install claude --scope user",
    "  npx -y @fingerskier/langer install grok --scope user",
    "  npx -y @fingerskier/langer ensure",
    "  npx -y @fingerskier/langer tools list",
    "  npx -y @fingerskier/langer tools ensure typescript",
    "  npx -y @fingerskier/langer tools update",
    "",
    "Binary releases: https://github.com/fingerskier/langer/releases",
    "Docs: https://github.com/fingerskier/langer#install",
  ].join("\n");
}

function parseInstallArgs(args) {
  const target = args[0];
  if (!target || target.startsWith("-")) {
    throw new Error("install requires a target: claude | grok | codex | all");
  }
  if (!["claude", "grok", "codex", "all"].includes(target)) {
    throw new Error(`unknown install target: ${target}`);
  }

  /** @type {Record<string, unknown>} */
  const opts = { target, scope: "user" };
  for (let i = 1; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--scope") {
      const value = args[++i];
      if (value !== "user" && value !== "repo") {
        throw new Error("--scope must be user or repo");
      }
      opts.scope = value;
    } else if (arg === "--binary") {
      opts.binary = args[++i];
      if (!opts.binary) throw new Error("--binary requires a path");
    } else if (arg === "--version") {
      opts.version = args[++i];
      if (!opts.version) throw new Error("--version requires a value");
    } else if (arg === "--dry-run") {
      opts.dryRun = true;
    } else if (arg === "--force") {
      opts.force = true;
    } else {
      throw new Error(`unknown install option: ${arg}`);
    }
  }
  return opts;
}

function parseEnsureArgs(args) {
  /** @type {Record<string, unknown>} */
  const opts = {};
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--binary") {
      opts.binary = args[++i];
    } else if (arg === "--version") {
      opts.version = args[++i];
    } else if (arg === "--dry-run") {
      opts.dryRun = true;
    } else if (arg === "--force") {
      opts.force = true;
    } else {
      throw new Error(`unknown ensure option: ${arg}`);
    }
  }
  return opts;
}
