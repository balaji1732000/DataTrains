import { createHash } from "node:crypto";
import { readdir, readFile, stat } from "node:fs/promises";
import { posix, resolve, sep } from "node:path";
import { validateTrajectory } from "@trajectory/schema";

type ReleaseFile = { path: string; size: number; sha256: string; media_type: string };
type RedactionReference = { session_id: string; plan_id: string; version: number; plan_hash: string };
type ReleaseManifest = {
  schema_version: string;
  release_id: string;
  project_id: string;
  name: string;
  release_profile: string;
  trajectory_schema: string;
  exporter_version: string;
  pipeline_version: string;
  configuration_hash: string;
  source_session_ids: string[];
  redaction_plans: RedactionReference[];
  files: ReleaseFile[];
};

export type ReleaseValidationResult =
  | { valid: true; releaseID: string; trajectories: number }
  | { valid: false; errors: string[] };

export async function validateReleaseDirectory(directory: string): Promise<ReleaseValidationResult> {
  const errors: string[] = [];
  const root = resolve(directory);
  let manifest: ReleaseManifest;
  try {
    manifest = JSON.parse(await readFile(resolve(root, "manifest.json"), "utf8")) as ReleaseManifest;
  } catch (error) {
    return { valid: false, errors: [`manifest.json: ${messageFor(error)}`] };
  }
  try {
    validateManifestShape(manifest, errors);
  } catch (error) {
    errors.push(`manifest structure: ${messageFor(error)}`);
  }
  if (errors.length > 0) return { valid: false, errors };

  const configuration = {
    project_id: manifest.project_id,
    name: manifest.name,
    release_profile: manifest.release_profile,
    trajectory_schema: manifest.trajectory_schema,
    exporter_version: manifest.exporter_version,
    pipeline_version: manifest.pipeline_version,
    source_session_ids: manifest.source_session_ids,
    redaction_plans: manifest.redaction_plans,
  };
  const configurationHash = sha256(Buffer.from(JSON.stringify(configuration)));
  if (manifest.configuration_hash !== configurationHash) errors.push("configuration_hash does not match the manifest configuration");
  if (manifest.release_id !== `rel_${configurationHash.slice(0, 32)}`) errors.push("release_id is not derived from configuration_hash");

  const individual = new Map<string, string>();
  const releaseFiles = new Map<string, Buffer>();
  for (const file of manifest.files) {
    const filename = containedPath(root, file.path);
    if (!filename) {
      errors.push(`unsafe release path: ${file.path}`);
      continue;
    }
    try {
      const contents = await readFile(filename);
      releaseFiles.set(file.path, contents);
      if (contents.byteLength !== file.size) errors.push(`size mismatch: ${file.path}`);
      if (sha256(contents) !== file.sha256) errors.push(`SHA-256 mismatch: ${file.path}`);
      if (file.path.startsWith("videos/") && !isMP4(contents)) errors.push(`${file.path}: does not contain an ISO BMFF/MP4 header`);
      if (file.path.startsWith("trajectories/") && file.path.endsWith(".json")) {
        const text = contents.toString("utf8").trim();
        const trajectory: unknown = JSON.parse(text);
        const result = validateTrajectory(trajectory);
        if (!result.valid) errors.push(...result.errors.map((error) => `${file.path}: ${error}`));
        const sessionID = file.path.slice("trajectories/".length, -".json".length);
        if (provenanceSessionID(trajectory) !== sessionID) errors.push(`${file.path}: provenance.session_id does not match filename`);
        individual.set(sessionID, text);
      }
    } catch (error) {
      errors.push(`${file.path}: ${messageFor(error)}`);
    }
  }

  const expectedChecksums = manifest.files
    .filter((file) => file.path !== "checksums.sha256")
    .map((file) => `${file.sha256}  ${file.path}\n`)
    .join("");
  if (releaseFiles.get("checksums.sha256")?.toString("utf8") !== expectedChecksums) {
    errors.push("checksums.sha256 does not exactly match the declared release files");
  }
  const readme = releaseFiles.get("README.md")?.toString("utf8").trim();
  if (!readme) errors.push("README.md is missing or empty");
  try {
    const schema = JSON.parse(releaseFiles.get("schema.json")?.toString("utf8") ?? "") as { $id?: unknown };
    if (schema.$id !== "https://trajectory.local/schemas/trajectory-v1.json") errors.push("schema.json is not trajectory/v1");
  } catch (error) {
    errors.push(`schema.json: ${messageFor(error)}`);
  }

  const jsonlPath = containedPath(root, "trajectories.jsonl");
  if (jsonlPath) {
    try {
      const lines = (await readFile(jsonlPath, "utf8")).trim().split("\n").filter(Boolean);
      if (lines.length !== manifest.source_session_ids.length) errors.push("trajectories.jsonl line count does not match source_session_ids");
      lines.forEach((line, index) => {
        try {
          const trajectory: unknown = JSON.parse(line);
          const result = validateTrajectory(trajectory);
          if (!result.valid) errors.push(...result.errors.map((error) => `trajectories.jsonl:${index + 1}: ${error}`));
          const sessionID = provenanceSessionID(trajectory);
          if (sessionID !== manifest.source_session_ids[index]) errors.push(`trajectories.jsonl:${index + 1}: unexpected session order`);
          if (sessionID && individual.get(sessionID) !== line) errors.push(`trajectories.jsonl:${index + 1}: differs from individual trajectory file`);
        } catch (error) {
          errors.push(`trajectories.jsonl:${index + 1}: ${messageFor(error)}`);
        }
      });
    } catch (error) {
      errors.push(`trajectories.jsonl: ${messageFor(error)}`);
    }
  }

  for (const sessionID of manifest.source_session_ids) {
    if (!individual.has(sessionID)) errors.push(`missing individual trajectory for ${sessionID}`);
  }
  try {
    const actualFiles = (await walk(root)).filter((file) => file !== "manifest.json").sort();
    const declaredFiles = manifest.files.map((file) => file.path);
    if (JSON.stringify(actualFiles) !== JSON.stringify(declaredFiles)) errors.push("release contains undeclared files or is missing declared files");
  } catch (error) {
    errors.push(`release inventory: ${messageFor(error)}`);
  }
  return errors.length > 0
    ? { valid: false, errors: [...new Set(errors)].sort() }
    : { valid: true, releaseID: manifest.release_id, trajectories: manifest.source_session_ids.length };
}

function validateManifestShape(value: ReleaseManifest, errors: string[]) {
  if (!value || typeof value !== "object") {
    errors.push("manifest must be an object");
    return;
  }
  if (value.schema_version !== "release/v1") errors.push("schema_version must be release/v1");
  for (const [name, field] of Object.entries({
    release_id: value.release_id,
    project_id: value.project_id,
    name: value.name,
    exporter_version: value.exporter_version,
    pipeline_version: value.pipeline_version,
  })) {
    if (typeof field !== "string" || field.length === 0) errors.push(`${name} must be a non-empty string`);
  }
  if (value.trajectory_schema !== "trajectory/v1") errors.push("trajectory_schema must be trajectory/v1");
  if (value.release_profile !== "trajectory_only" && value.release_profile !== "redacted_video") {
    errors.push("release_profile must be trajectory_only or redacted_video");
  }
  if (!isHash(value.configuration_hash)) errors.push("configuration_hash must be a SHA-256 digest");
  if (!Array.isArray(value.source_session_ids) || value.source_session_ids.length === 0) errors.push("source_session_ids must be non-empty");
  else if (!sortedUnique(value.source_session_ids)) errors.push("source_session_ids must be sorted and unique");
  if (!Array.isArray(value.redaction_plans)) {
    errors.push("redaction_plans must be an array");
  } else {
    if (!sortedUnique(value.redaction_plans.map((plan) => plan.session_id))) errors.push("redaction_plans must be sorted and unique by session_id");
    for (const plan of value.redaction_plans) {
      if (typeof plan.plan_id !== "string" || plan.plan_id.length === 0 || !Number.isSafeInteger(plan.version) || plan.version < 1 || !isHash(plan.plan_hash)) {
        errors.push(`invalid redaction plan reference for ${plan.session_id}`);
      }
    }
    if (value.release_profile === "trajectory_only" && value.redaction_plans.length !== 0) {
      errors.push("trajectory_only releases cannot declare redaction plans");
    }
    if (value.release_profile === "redacted_video" && JSON.stringify(value.redaction_plans.map((plan) => plan.session_id)) !== JSON.stringify(value.source_session_ids)) {
      errors.push("redacted_video releases require one redaction plan for every source session");
    }
  }
  if (!Array.isArray(value.files) || value.files.length === 0) {
    errors.push("files must be non-empty");
    return;
  }
  if (!sortedUnique(value.files.map((file) => file.path))) errors.push("files must be sorted and unique by path");
  for (const file of value.files) {
    if (!safeRelativePath(file.path)) errors.push(`unsafe release path: ${file.path}`);
    if (!Number.isSafeInteger(file.size) || file.size < 1) errors.push(`invalid file size: ${file.path}`);
    if (!isHash(file.sha256)) errors.push(`invalid file SHA-256: ${file.path}`);
    if (typeof file.media_type !== "string" || file.media_type.length === 0) errors.push(`invalid file media type: ${file.path}`);
	const expectedMediaType = mediaTypeForPath(file.path);
	if (expectedMediaType && file.media_type !== expectedMediaType) errors.push(`invalid media type for ${file.path}: expected ${expectedMediaType}`);
  }
  if (!value.files.some((file) => file.path === "trajectories.jsonl")) errors.push("files must include trajectories.jsonl");
  for (const required of ["README.md", "schema.json", "checksums.sha256"]) {
    if (!value.files.some((file) => file.path === required)) errors.push(`files must include ${required}`);
  }
	const videoNamespace = value.files.filter((file) => file.path.startsWith("videos/"));
	const videoFiles = videoNamespace.filter((file) => file.path.endsWith(".mp4"));
	for (const file of videoNamespace) {
		const parts = file.path.split("/");
		if (parts.length !== 3 || !value.source_session_ids.includes(parts[1] ?? "") || !/^[A-Za-z0-9_-]+\.mp4$/.test(parts[2] ?? "")) {
			errors.push(`invalid redacted video path: ${file.path}`);
		}
	}
  if (value.release_profile === "trajectory_only" && videoFiles.length !== 0) errors.push("trajectory_only releases cannot include videos");
  if (value.release_profile === "redacted_video") {
    for (const sessionID of value.source_session_ids) {
      if (!videoFiles.some((file) => file.path.startsWith(`videos/${sessionID}/`))) errors.push(`missing redacted video for ${sessionID}`);
    }
  }
}

function mediaTypeForPath(path: string) {
	if (path === "README.md") return "text/markdown";
	if (path === "schema.json") return "application/schema+json";
	if (path === "checksums.sha256") return "text/plain";
	if (path === "trajectories.jsonl") return "application/x-ndjson";
	if (path.startsWith("trajectories/") && path.endsWith(".json")) return "application/json";
	if (path.startsWith("videos/") && path.endsWith(".mp4")) return "video/mp4";
	return null;
}

function safeRelativePath(value: string) {
  return typeof value === "string" && value !== "" && !value.startsWith("/") && !value.includes("\\") && posix.normalize(value) === value && value !== "." && !value.startsWith("../");
}

function containedPath(root: string, relative: string) {
  if (!safeRelativePath(relative)) return null;
  const filename = resolve(root, ...relative.split("/"));
  return filename.startsWith(root + sep) ? filename : null;
}

function sortedUnique(values: string[]) {
  return values.every((value, index) => index === 0 || values[index - 1]! < value);
}

function isHash(value: unknown): value is string {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function sha256(contents: Buffer) {
  return createHash("sha256").update(contents).digest("hex");
}

function isMP4(contents: Buffer) {
  return contents.byteLength >= 12 && contents.subarray(4, 8).toString("ascii") === "ftyp";
}

function provenanceSessionID(value: unknown) {
  if (!value || typeof value !== "object" || !("provenance" in value)) return null;
  const provenance = value.provenance;
  if (!provenance || typeof provenance !== "object" || !("session_id" in provenance)) return null;
  return typeof provenance.session_id === "string" ? provenance.session_id : null;
}

async function walk(root: string, directory = root): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  const files: string[] = [];
  for (const entry of entries) {
    const filename = resolve(directory, entry.name);
    if (entry.isDirectory()) files.push(...await walk(root, filename));
    else if ((await stat(filename)).isFile()) files.push(filename.slice(root.length + 1).split(sep).join("/"));
  }
  return files;
}

function messageFor(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}
