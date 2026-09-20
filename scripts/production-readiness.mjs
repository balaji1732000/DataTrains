#!/usr/bin/env node

import { access, readFile } from "node:fs/promises";
import { constants } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const ledgerPath = resolve(repositoryRoot, "docs/operations/production-readiness.json");
const allowedStatuses = new Set([
  "complete",
  "external_evidence_required",
  "physical_evidence_required",
  "decision_required",
  "authorization_required",
  "approval_required",
  "not_started"
]);

function fail(message) {
  process.stderr.write(`Production-readiness ledger error: ${message}\n`);
  process.exitCode = 2;
}

async function main() {
  const ledger = JSON.parse(await readFile(ledgerPath, "utf8"));
  if (ledger.schema_version !== 1 || !Array.isArray(ledger.gates) || ledger.gates.length === 0) {
    fail("expected schema_version 1 and at least one gate");
    return;
  }

  const ids = new Set();
  const missingEvidence = [];
  let invalid = false;
  for (const gate of ledger.gates) {
    if (!gate.id || ids.has(gate.id)) {
      fail(`gate IDs must be present and unique: ${gate.id ?? "<missing>"}`);
      invalid = true;
      continue;
    }
    ids.add(gate.id);
    if (!allowedStatuses.has(gate.status)) {
      fail(`${gate.id} has unsupported status ${gate.status}`);
      invalid = true;
    }
    if (!Array.isArray(gate.local_evidence) || gate.local_evidence.length === 0) {
      fail(`${gate.id} has no local evidence references`);
      invalid = true;
    }
    if (gate.status !== "complete" && (!Array.isArray(gate.remaining) || gate.remaining.length === 0)) {
      fail(`${gate.id} is open but has no remaining work`);
      invalid = true;
    }
    for (const relativePath of gate.local_evidence ?? []) {
      const absolutePath = resolve(repositoryRoot, relativePath);
      if (!absolutePath.startsWith(`${repositoryRoot}/`)) {
        fail(`${gate.id} evidence escapes the repository: ${relativePath}`);
        invalid = true;
        continue;
      }
      try {
        await access(absolutePath, constants.R_OK);
      } catch {
        missingEvidence.push(`${gate.id}: ${relativePath}`);
      }
    }
  }

  if (missingEvidence.length > 0) {
    fail(`missing local evidence:\n- ${missingEvidence.join("\n- ")}`);
    invalid = true;
  }
  if (invalid) return;

  const open = ledger.gates.filter((gate) => gate.status !== "complete");
  const report = {
    product: ledger.product,
    production_ready: open.length === 0,
    complete: ledger.gates.length - open.length,
    total: ledger.gates.length,
    gates: ledger.gates.map(({ id, title, status, remaining }) => ({ id, title, status, remaining }))
  };

  if (process.argv.includes("--json")) {
    process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  } else {
    process.stdout.write(`${report.product}: ${report.complete}/${report.total} production gates complete\n`);
    for (const gate of report.gates) {
      const marker = gate.status === "complete" ? "PASS" : "OPEN";
      process.stdout.write(`[${marker}] ${gate.id}: ${gate.status} — ${gate.title}\n`);
    }
  }

  if (process.argv.includes("--assert-ready") && !report.production_ready) {
    process.stderr.write("Production promotion refused: one or more gates remain open.\n");
    process.exitCode = 1;
  }
}

await main();
