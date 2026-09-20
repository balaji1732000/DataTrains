#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { validateTrajectory } from "@trajectory/schema";
import { validateReleaseDirectory } from "./release.js";

const input = process.argv[2];
if (!input) {
  console.error("Usage: trajectory-validator <trajectory.json> | --release <release-directory>");
  process.exitCode = 2;
} else if (input === "--release") {
  const directory = process.argv[3];
  if (!directory) {
    console.error("Usage: trajectory-validator --release <release-directory>");
    process.exitCode = 2;
  } else {
    const invocationDirectory = process.env.INIT_CWD ?? process.cwd();
    const result = await validateReleaseDirectory(resolve(invocationDirectory, directory));
    if (result.valid) console.log(`VALID RELEASE\nrelease: ${result.releaseID}\ntrajectories: ${result.trajectories}`);
    else {
      console.error(`INVALID RELEASE\n${result.errors.join("\n")}`);
      process.exitCode = 1;
    }
  }
} else {
  try {
    const invocationDirectory = process.env.INIT_CWD ?? process.cwd();
    const value: unknown = JSON.parse(await readFile(resolve(invocationDirectory, input), "utf8"));
    const result = validateTrajectory(value);
    if (result.valid) {
      console.log(`VALID\nschema: ${result.schemaVersion}`);
    } else {
      console.error(`INVALID\n${result.errors.join("\n")}`);
      process.exitCode = 1;
    }
  } catch (error) {
    console.error(`INVALID\n${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  }
}
