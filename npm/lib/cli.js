import { ensureBinary } from "./download.js";
import { installIntegration } from "./install.js";
import { binaryPath, packageVersion, releaseAsset, releaseTag } from "./paths.js";

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

  console.error(`Unknown command: ${command}`);
  console.error("");
  console.error(helpText());
  process.exitCode = 1;
}

export function helpText() {
  return [
    "langer-install — download langer binaries and wire MCP hosts",
    "",
    "USAGE",
    "  npx -y @fingerskier/langer <command> [options]",
    "",
    "COMMANDS",
    "  install <claude|grok|codex|all>   Download the binary (if needed) and register MCP",
    "  ensure                            Download/install the host binary under ~/.langer/bin",
    "  path                              Print the binary install path",
    "  asset                             Print the GitHub release asset name for this host",
    "  version                           Print this npm package version",
    "  help                              Show this help",
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
    "  npx -y @fingerskier/langer install all --scope user --dry-run",
    "  npx -y @fingerskier/langer ensure",
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
