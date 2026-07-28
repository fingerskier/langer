import test from "node:test";
import assert from "node:assert/strict";
import { helpText } from "../lib/cli.js";

test("help documents tools proxy and binary ensure distinction", () => {
  const text = helpText();
  assert.match(text, /tools list\|ensure/);
  assert.match(text, /tools ensure typescript/);
  assert.match(text, /ensure\s+Download\/install the host binary/);
  assert.match(text, /BINARY COMMANDS/);
});
