import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

test("CLI prints the V1 validity contract", () => {
  const fixture = fileURLToPath(new URL("../../../packages/trajectory-schema/fixtures/valid-trajectory.json", import.meta.url));
  const result = spawnSync(process.execPath, ["dist/cli.js", fixture], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^VALID\nschema: trajectory\/v1/m);
});

test("CLI verifies a complete deterministic release", async () => {
  const directory = join(tmpdir(), `trajectory-release-${process.pid}-${Date.now()}`);
  await mkdir(join(directory, "trajectories"), { recursive: true });
  const fixturePath = fileURLToPath(new URL("../../../packages/trajectory-schema/fixtures/valid-trajectory.json", import.meta.url));
  const fixture = JSON.parse(await readFile(fixturePath, "utf8"));
  fixture.provenance.session_id = "sess_fixture";
  fixture.qa.state = "accepted";
  fixture.privacy.pii_review = "passed";
  const trajectory = `${JSON.stringify(fixture)}\n`;
  await writeFile(join(directory, "trajectories", "sess_fixture.json"), trajectory);
  await writeFile(join(directory, "trajectories.jsonl"), trajectory);
  const readme = "# Fixture release\n";
  const schema = await readFile(fileURLToPath(new URL("../../../packages/trajectory-schema/schemas/trajectory-v1.json", import.meta.url)));
  await writeFile(join(directory, "README.md"), readme);
  await writeFile(join(directory, "schema.json"), schema);
  const payloadFiles = [
    fileEntry("README.md", readme, "text/markdown"),
    fileEntry("schema.json", schema, "application/schema+json"),
    fileEntry("trajectories.jsonl", trajectory, "application/x-ndjson"),
    fileEntry("trajectories/sess_fixture.json", trajectory, "application/json"),
  ].sort(comparePaths);
  const checksums = payloadFiles.map((file) => `${file.sha256}  ${file.path}\n`).join("");
  await writeFile(join(directory, "checksums.sha256"), checksums);
  const files = [...payloadFiles, fileEntry("checksums.sha256", checksums, "text/plain")].sort(comparePaths);
  const configuration = {
    project_id: "proj_fixture",
    name: "fixture-v1",
    release_profile: "trajectory_only",
    trajectory_schema: "trajectory/v1",
    exporter_version: "trajectory-exporter/0.1.0",
    pipeline_version: "trajectory-pipeline/0.1.0",
    source_session_ids: ["sess_fixture"],
    redaction_plans: [],
  };
  const configurationHash = digest(JSON.stringify(configuration));
  const manifest = {
    schema_version: "release/v1",
    release_id: `rel_${configurationHash.slice(0, 32)}`,
    ...configuration,
    configuration_hash: configurationHash,
    files,
  };
  await writeFile(join(directory, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
  const result = spawnSync(process.execPath, ["dist/cli.js", "--release", directory], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^VALID RELEASE/m);

  await writeFile(join(directory, "trajectories.jsonl"), `${trajectory}tampered\n`);
  const tampered = spawnSync(process.execPath, ["dist/cli.js", "--release", directory], { encoding: "utf8" });
  assert.equal(tampered.status, 1);
  assert.match(tampered.stderr, /SHA-256 mismatch/);
});

test("CLI verifies redacted-video inventory and rejects a non-MP4 payload", async () => {
  const directory = join(tmpdir(), `trajectory-video-release-${process.pid}-${Date.now()}`);
  await mkdir(join(directory, "trajectories"), { recursive: true });
  await mkdir(join(directory, "videos", "sess_fixture"), { recursive: true });
  const fixturePath = fileURLToPath(new URL("../../../packages/trajectory-schema/fixtures/valid-trajectory.json", import.meta.url));
  const fixture = JSON.parse(await readFile(fixturePath, "utf8"));
  fixture.provenance.session_id = "sess_fixture";
  fixture.qa.state = "accepted";
  fixture.privacy.pii_review = "passed";
  const trajectory = `${JSON.stringify(fixture)}\n`;
  const readme = "# Redacted video release\n";
  const schema = await readFile(fileURLToPath(new URL("../../../packages/trajectory-schema/schemas/trajectory-v1.json", import.meta.url)));
  const video = Buffer.from([0, 0, 0, 16, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x6d]);
  await writeFile(join(directory, "trajectories", "sess_fixture.json"), trajectory);
  await writeFile(join(directory, "trajectories.jsonl"), trajectory);
  await writeFile(join(directory, "README.md"), readme);
  await writeFile(join(directory, "schema.json"), schema);
  await writeFile(join(directory, "videos", "sess_fixture", "000001.mp4"), video);
  const payloadFiles = [
    fileEntry("README.md", readme, "text/markdown"),
    fileEntry("schema.json", schema, "application/schema+json"),
    fileEntry("trajectories.jsonl", trajectory, "application/x-ndjson"),
    fileEntry("trajectories/sess_fixture.json", trajectory, "application/json"),
    fileEntry("videos/sess_fixture/000001.mp4", video, "video/mp4"),
  ].sort(comparePaths);
  const checksums = payloadFiles.map((file) => `${file.sha256}  ${file.path}\n`).join("");
  await writeFile(join(directory, "checksums.sha256"), checksums);
  const files = [...payloadFiles, fileEntry("checksums.sha256", checksums, "text/plain")].sort(comparePaths);
  const configuration = {
    project_id: "proj_fixture",
    name: "fixture-redacted-v1",
    release_profile: "redacted_video",
    trajectory_schema: "trajectory/v1",
    exporter_version: "trajectory-exporter/0.3.0",
    pipeline_version: "trajectory-pipeline/0.1.0",
    source_session_ids: ["sess_fixture"],
    redaction_plans: [{ session_id: "sess_fixture", plan_id: "redaction_fixture", version: 1, plan_hash: "a".repeat(64) }],
  };
  const configurationHash = digest(JSON.stringify(configuration));
  await writeFile(join(directory, "manifest.json"), `${JSON.stringify({
    schema_version: "release/v1",
    release_id: `rel_${configurationHash.slice(0, 32)}`,
    ...configuration,
    configuration_hash: configurationHash,
    files,
  }, null, 2)}\n`);
  const result = spawnSync(process.execPath, ["dist/cli.js", "--release", directory], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);

  await writeFile(join(directory, "videos", "sess_fixture", "000001.mp4"), Buffer.from("not an mp4!!"));
  const invalid = spawnSync(process.execPath, ["dist/cli.js", "--release", directory], { encoding: "utf8" });
  assert.equal(invalid.status, 1);
  assert.match(invalid.stderr, /ISO BMFF\/MP4 header/);
});

function fileEntry(path, contents, mediaType) {
  return { path, size: Buffer.byteLength(contents), sha256: digest(contents), media_type: mediaType };
}

function digest(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function comparePaths(left, right) {
  return left.path < right.path ? -1 : left.path > right.path ? 1 : 0;
}
