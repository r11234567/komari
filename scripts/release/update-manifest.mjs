import { createHash, createPrivateKey, sign } from "node:crypto";
import { createReadStream } from "node:fs";
import { readdir, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";

const [directory] = process.argv.slice(2);
const { VERSION: version, VERSION_HASH: versionHash } = process.env;
if (!directory || !version || !/^[a-zA-Z0-9]{7}$/.test(versionHash ?? "")) {
  throw new Error("Update manifest requires an asset directory, VERSION and VERSION_HASH.");
}

const encodedKey = process.env.KOMARI_UPDATE_SIGNING_KEY?.trim();
let key;
if (encodedKey) {
  const der = Buffer.from(encodedKey, "base64");
  if (der.toString("base64") !== encodedKey) {
    throw new Error("KOMARI_UPDATE_SIGNING_KEY must be base64-encoded PKCS#8.");
  }
  key = createPrivateKey({ key: der, format: "der", type: "pkcs8" });
  if (key.asymmetricKeyType !== "ed25519") {
    throw new Error("KOMARI_UPDATE_SIGNING_KEY must be an Ed25519 key.");
  }
}

const pattern = /^komari-(linux|windows)-([a-zA-Z0-9]+)(?:\.exe)?$/;
const assets = [];
for (const name of (await readdir(directory)).sort()) {
  const match = name.match(pattern);
  if (!match) continue;
  const path = join(directory, name);
  const file = await stat(path);
  if (!file.isFile() || file.size === 0) {
    throw new Error(`Invalid update asset: ${name}`);
  }
  const hash = createHash("sha256");
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  assets.push({
    name, os: match[1], arch: match[2], size: file.size, sha256: hash.digest("hex"),
  });
}
if (assets.length === 0) {
  throw new Error("No update binaries found.");
}

const content = Buffer.from(JSON.stringify({
  schema: 1, version, version_hash: versionHash, assets,
}, null, 2) + "\n");
await writeFile(join(directory, "komari-update.json"), content);
if (key) {
  await writeFile(join(directory, "komari-update.json.sig"), sign(null, content, key));
} else {
  process.stderr.write("Update signing key unavailable. This build cannot be installed by panel self-update.\n");
}
