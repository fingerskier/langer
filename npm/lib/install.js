import path from "node:path";
import { homedir } from "node:os";
import { ensureBinary } from "./download.js";
import {
  langerMcpServer,
  mergeMcpServersJson,
  readOptional,
  readOptionalJson,
  upsertTomlMcpServer,
  writeFileEnsured,
} from "./mcp-config.js";
import { toForwardSlashes } from "./paths.js";

/**
 * @typedef {"claude" | "grok" | "codex" | "all"} InstallTarget
 * @typedef {"user" | "repo"} InstallScope
 */

/**
 * @param {{
 *   target: InstallTarget,
 *   scope?: InstallScope,
 *   dryRun?: boolean,
 *   force?: boolean,
 *   binary?: string,
 *   version?: string,
 *   cwd?: string,
 *   home?: string,
 * }} options
 */
export async function installIntegration(options) {
  const context = {
    cwd: path.resolve(options.cwd ?? process.cwd()),
    home: path.resolve(options.home ?? homedir()),
    dryRun: options.dryRun ?? false,
    force: options.force ?? false,
  };

  const binary = await ensureBinary({
    home: context.home,
    dryRun: context.dryRun,
    force: context.force,
    binary: options.binary,
    version: options.version,
  });
  const server = langerMcpServer(binary);

  const summary = [];
  const files = [];

  if (context.dryRun) {
    summary.push(`Would ensure binary at ${binary}`);
  } else {
    summary.push(`Binary ready: ${binary}`);
  }

  const targets =
    options.target === "all" ? ["claude", "grok", "codex"] : [options.target];

  for (const target of targets) {
    let plan;
    if (target === "claude") plan = await installClaude(server, { ...options, ...context });
    else if (target === "grok") plan = await installGrok(server, { ...options, ...context });
    else if (target === "codex") plan = await installCodex(server, { ...options, ...context });
    else throw new Error(`unknown install target: ${target}`);
    summary.push(...plan.summary);
    files.push(...plan.files);
  }

  summary.push(
    "Restart the agent (Claude/Codex) or press `r` in Grok `/mcps` so the MCP server loads.",
  );
  return { summary, files, binary };
}

async function installClaude(server, options) {
  const configPath =
    options.scope === "repo"
      ? path.join(options.cwd, ".mcp.json")
      : path.join(options.home, ".claude.json");

  const existing = await readOptionalJson(configPath);
  const merged = mergeMcpServersJson(existing, {
    command: server.command,
    args: server.args,
  });
  const content = `${JSON.stringify(merged, null, 2)}\n`;

  if (!options.dryRun) {
    await writeFileEnsured(configPath, content);
  }

  const verb = options.dryRun ? "Would register" : "Registered";
  return {
    summary: [
      `${verb} Claude MCP server 'langer' (${options.scope} scope) in ${configPath}.`,
      options.scope === "user"
        ? "Tip: `claude mcp add langer -- <binary> mcp --stdio` also works if you prefer the CLI."
        : "Repo-scoped Claude uses .mcp.json at the project root.",
    ],
    files: [configPath],
  };
}

async function installGrok(server, options) {
  const configPath =
    options.scope === "repo"
      ? path.join(options.cwd, ".grok", "config.toml")
      : path.join(options.home, ".grok", "config.toml");

  const existing = await readOptional(configPath);
  const next = upsertTomlMcpServer(existing, {
    command: toForwardSlashes(server.command),
    args: server.args,
  });

  if (!options.dryRun) {
    await writeFileEnsured(configPath, next.endsWith("\n") ? next : `${next}\n`);
  }

  const verb = options.dryRun ? "Would register" : "Registered";
  return {
    summary: [
      `${verb} Grok MCP server 'langer' (${options.scope} scope) in ${configPath}.`,
    ],
    files: [configPath],
  };
}

async function installCodex(server, options) {
  // Codex honors MCP entries in config.toml under [mcp_servers.*].
  const configPath =
    options.scope === "repo"
      ? path.join(options.cwd, ".codex", "config.toml")
      : path.join(options.home, ".codex", "config.toml");

  const existing = await readOptional(configPath);
  const next = upsertTomlMcpServer(existing, {
    command: toForwardSlashes(server.command),
    args: server.args,
  });

  if (!options.dryRun) {
    await writeFileEnsured(configPath, next.endsWith("\n") ? next : `${next}\n`);
  }

  const verb = options.dryRun ? "Would register" : "Registered";
  return {
    summary: [
      `${verb} Codex MCP server 'langer' (${options.scope} scope) in ${configPath}.`,
    ],
    files: [configPath],
  };
}
