import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, verify } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

const signer = fileURLToPath(new URL("./update-manifest.mjs", import.meta.url));
const binary = Buffer.from("release build fixture");
const binaryNames = ["komari-linux-amd64", "komari-windows-amd64.exe"];

function runSigner(t, version, build, signingKey) {
  const directory = mkdtempSync(join(tmpdir(), "komari-update-manifest-"));
  const manifest = join(directory, "komari-update.json");
  const signature = join(directory, "komari-update.json.sig");
  t.after(() => {
    for (const path of [manifest, signature, ...binaryNames.map(name => join(directory, name))]) {
      rmSync(path, { force: true });
    }
    rmdirSync(directory);
  });
  for (const name of binaryNames) writeFileSync(join(directory, name), binary);
  const result = spawnSync(process.execPath, [signer, directory], {
    env: { ...process.env, VERSION: version, VERSION_HASH: build, KOMARI_UPDATE_SIGNING_KEY: signingKey },
    encoding: "utf8",
  });
  return { result, manifest, signature };
}

test("release, snapshot and rebuilt release manifests sign exact asset metadata", t => {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const secret = privateKey.export({ format: "der", type: "pkcs8" }).toString("base64");
  for (const [version, build] of [["2.0.5", "def4567"], ["Snapshot-2611250000", "abc12d3"], ["2.0.5", "fed7654"]]) {
    const { result, manifest, signature } = runSigner(t, version, build, secret);
    assert.equal(result.status, 0, result.stderr);
    const bytes = readFileSync(manifest);
    const proof = readFileSync(signature);
    assert.equal(proof.length, 64);
    assert(verify(null, bytes, publicKey, proof));
    assert(!verify(null, Buffer.concat([bytes, Buffer.from(" ")]), publicKey, proof));
    const data = JSON.parse(bytes);
    assert.equal(data.version, version);
    assert.equal(data.version_hash, build);
    assert.equal(data.assets.length, 2);
    for (const asset of data.assets) {
      assert.equal(asset.size, binary.length);
      assert.equal(asset.sha256, createHash("sha256").update(binary).digest("hex"));
    }
  }
});

test("an unconfigured publisher emits no signature and rejects a malformed key", t => {
  const unsigned = runSigner(t, "2.0.5", "abc12d3", "");
  assert.equal(unsigned.result.status, 0);
  assert(existsSync(unsigned.manifest));
  assert(!existsSync(unsigned.signature));

  const invalid = runSigner(t, "2.0.5", "abc12d3", "not-a-release-key");
  assert.notEqual(invalid.result.status, 0);
  assert(!existsSync(invalid.manifest));
  assert(!existsSync(invalid.signature));
});
