import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

const repositoryRoot = process.cwd();
const bundleRoot = path.join(
  repositoryRoot,
  "apps",
  "collector",
  "src-tauri",
  "target",
  "release",
  "bundle",
);
const releaseExtensions = new Set([".deb", ".AppImage", ".exe", ".dmg"]);

async function visit(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...(await visit(absolute)));
    if (entry.isFile() && releaseExtensions.has(path.extname(entry.name))) {
      files.push(absolute);
    }
  }
  return files;
}

const files = (await visit(bundleRoot)).sort();
if (files.length === 0) {
  throw new Error(`no release artifacts found under ${bundleRoot}`);
}

for (const file of files) {
  const digest = createHash("sha256").update(await readFile(file)).digest("hex");
  console.log(`${digest}  ${path.relative(repositoryRoot, file)}`);
}
