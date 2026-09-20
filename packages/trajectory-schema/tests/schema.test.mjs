import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { URL } from "node:url";
import { validateTrajectory } from "../dist/index.js";

async function fixture(name) {
  return JSON.parse(await readFile(new URL(`../fixtures/${name}`, import.meta.url), "utf8"));
}

test("accepts the canonical V1 fixture", async () => {
  assert.deepEqual(validateTrajectory(await fixture("valid-trajectory.json")), {
    valid: true,
    schemaVersion: "trajectory/v1",
  });
});

test("reports missing fields and unknown action types", async () => {
  const result = validateTrajectory(await fixture("invalid-trajectory.json"));
  assert.equal(result.valid, false);
  assert.ok(result.errors.includes("missing: task.goal"));
  assert.ok(result.errors.some((error) => error.includes("invalid action type")));
});
