import { Ajv2020, type ErrorObject } from "ajv/dist/2020.js";
import trajectoryV1 from "../schemas/trajectory-v1.json" with { type: "json" };

export type ValidationResult =
  | { valid: true; schemaVersion: "trajectory/v1" }
  | { valid: false; errors: string[] };

const ajv = new Ajv2020({ allErrors: true, strict: true, validateFormats: false, verbose: true });
const validateV1 = ajv.compile(trajectoryV1);

function formatError(error: ErrorObject): string {
  if (error.keyword === "required") {
    const property = String(error.params.missingProperty);
    const parent = error.instancePath.replace(/^\//, "").replaceAll("/", ".");
    return `missing: ${parent ? `${parent}.` : ""}${property}`;
  }
  if (error.keyword === "enum" && error.instancePath.endsWith("/type")) {
    return `invalid action type at ${error.instancePath || "/"}: ${String(error.data)}`;
  }
  return `${error.instancePath || "/"}: ${error.message ?? error.keyword}`;
}

export function validateTrajectory(value: unknown): ValidationResult {
  if (validateV1(value)) {
    return { valid: true, schemaVersion: "trajectory/v1" };
  }
  return {
    valid: false,
    errors: (validateV1.errors ?? []).map(formatError),
  };
}

export { trajectoryV1 };
