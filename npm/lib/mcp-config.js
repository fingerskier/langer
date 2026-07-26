import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { toForwardSlashes } from "./paths.js";

/**
 * @typedef {{ command: string, args: string[] }} McpServer
 */

/** MCP server entry that runs the installed Go binary. */
export function langerMcpServer(binary) {
  return {
    command: binary,
    args: ["mcp", "--stdio"],
  };
}

export async function readOptional(file) {
  try {
    return await readFile(file, "utf8");
  } catch (err) {
    if (err && err.code === "ENOENT") return undefined;
    throw err;
  }
}

export async function readOptionalJson(file) {
  const text = await readOptional(file);
  if (text === undefined) return undefined;
  return JSON.parse(text);
}

export async function writeFileEnsured(file, content) {
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, content, "utf8");
}

export function mergeMcpServersJson(existing, server, name = "langer") {
  const base = existing && typeof existing === "object" ? existing : {};
  const current = base.mcpServers;
  const servers = current && typeof current === "object" ? { ...current } : {};
  servers[name] = server;
  return { ...base, mcpServers: servers };
}

/**
 * Upsert [mcp_servers.langer] (and nested tables) in a Grok/Codex-style TOML file.
 */
export function upsertTomlMcpServer(existing, server, name = "langer") {
  const section = formatTomlMcpSection(server, name);
  if (!existing || existing.trim().length === 0) {
    return section;
  }
  const without = removeTomlTables(existing, (header) => isMcpServerTable(header, name));
  const base = without.replace(/\s+$/u, "");
  return base.length > 0 ? `${base}\n\n${section}` : section;
}

function formatTomlMcpSection(server, name) {
  const command = toForwardSlashes(server.command);
  const args = server.args.map((a) => tomlString(a)).join(", ");
  return [
    `[mcp_servers.${name}]`,
    `command = ${tomlString(command)}`,
    `args = [${args}]`,
    "enabled = true",
    "startup_timeout_sec = 60",
    "",
  ].join("\n");
}

function tomlString(value) {
  return JSON.stringify(value);
}

function isMcpServerTable(header, name) {
  const key = `mcp_servers.${name}`;
  return header === key || header.startsWith(`${key}.`);
}

/**
 * Remove TOML tables whose header matches predicate. Headers are bare keys without brackets.
 */
export function removeTomlTables(text, predicate) {
  const lines = text.split(/\r?\n/);
  const out = [];
  let skipping = false;
  for (const line of lines) {
    const header = parseTomlTableHeader(line);
    if (header !== null) {
      skipping = predicate(header);
      if (skipping) continue;
      out.push(line);
      continue;
    }
    if (!skipping) out.push(line);
  }
  // Collapse excessive blank lines introduced by removals.
  return out.join("\n").replace(/\n{3,}/g, "\n\n");
}

function parseTomlTableHeader(line) {
  const trimmed = line.trim();
  const m = /^\[([^\]]+)\]\s*$/.exec(trimmed);
  return m ? m[1].trim() : null;
}
