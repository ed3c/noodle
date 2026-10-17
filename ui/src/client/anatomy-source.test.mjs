import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import test from "node:test";

test("vendored Anatomy core retains the reviewed upstream bytes", () => {
  const source = readFileSync(new URL("anatomy/iso-kit.mjs", import.meta.url));
  assert.equal(
    createHash("sha256").update(source).digest("hex"),
    "6d3ecc725d30c3b0346da309cf61dfa5a65017c23620bbd4093009d4fb104018",
    "Update the upstream pin and review its source before changing the geometry core.",
  );
});
