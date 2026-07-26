import test from "node:test";
import assert from "node:assert/strict";
import {
  mergeMcpServersJson,
  removeTomlTables,
  upsertTomlMcpServer,
} from "../lib/mcp-config.js";

test("mergeMcpServersJson preserves other servers", () => {
  const merged = mergeMcpServersJson(
    { mcpServers: { other: { command: "x" } }, foo: 1 },
    { command: "langer", args: ["mcp", "--stdio"] },
  );
  assert.equal(merged.foo, 1);
  assert.equal(merged.mcpServers.other.command, "x");
  assert.deepEqual(merged.mcpServers.langer, {
    command: "langer",
    args: ["mcp", "--stdio"],
  });
});

test("upsertTomlMcpServer inserts into empty file", () => {
  const text = upsertTomlMcpServer(undefined, {
    command: "C:/Users/me/.langer/bin/langer.exe",
    args: ["mcp", "--stdio"],
  });
  assert.match(text, /\[mcp_servers\.langer\]/);
  assert.match(text, /command = "C:\/Users\/me\/\.langer\/bin\/langer\.exe"/);
  assert.match(text, /args = \["mcp", "--stdio"\]/);
});

test("upsertTomlMcpServer replaces existing langer section only", () => {
  const existing = [
    "[something]",
    'a = "b"',
    "",
    "[mcp_servers.langer]",
    'command = "old"',
    'args = ["x"]',
    "",
    "[mcp_servers.other]",
    'command = "keep"',
    "",
  ].join("\n");

  const next = upsertTomlMcpServer(existing, {
    command: "/new/langer",
    args: ["mcp", "--stdio"],
  });
  assert.match(next, /\[something\]/);
  assert.match(next, /\[mcp_servers\.other\]/);
  assert.match(next, /command = "\/new\/langer"/);
  assert.doesNotMatch(next, /command = "old"/);
  assert.equal((next.match(/\[mcp_servers\.langer\]/g) || []).length, 1);
});

test("removeTomlTables drops nested tables", () => {
  const existing = [
    "[mcp_servers.langer]",
    'command = "x"',
    "[mcp_servers.langer.env]",
    'A = "1"',
    "[mcp_servers.keep]",
    'command = "y"',
  ].join("\n");
  const next = removeTomlTables(
    existing,
    (h) => h === "mcp_servers.langer" || h.startsWith("mcp_servers.langer."),
  );
  assert.doesNotMatch(next, /langer/);
  assert.match(next, /mcp_servers\.keep/);
});
