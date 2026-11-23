const { createHmac } = require('node:crypto');
const { setTimeout: delay } = require('node:timers/promises');

const periodMs = 30_000;
const submissionMarginMs = 5_000;

function totp(secret, counter) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const bits = [...secret.toUpperCase().replace(/=+$/, '')].map(c => {
    const n = alphabet.indexOf(c);
    if (n < 0) throw new Error('Invalid fixture TOTP secret');
    return n.toString(2).padStart(5, '0');
  }).join('');
  const key = Buffer.from((bits.match(/.{8}/g) || []).map(b => parseInt(b, 2)));
  const time = Buffer.alloc(8);
  time.writeBigUInt64BE(BigInt(counter));
  const digest = createHmac('sha1', key).update(time).digest();
  const offset = digest.at(-1) & 15;
  return String((digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, '0');
}

async function freshTOTP(secret, afterCounter) {
  if (!secret) throw new Error('Enable fixture TOTP before requesting a fresh code');
  // Leave time to submit the proof and never mint a code for a future step.
  while (true) {
    const now = Date.now();
    const counter = Math.floor(now / periodMs);
    const nearExpiry = now % periodMs >= periodMs - submissionMarginMs;
    const next = Math.max(afterCounter + 1, counter + Number(nearExpiry));
    if (next === counter) return { code: totp(secret, counter), counter };
    await delay(next * periodMs - now);
  }
}

function invalidTOTP(secret) {
  const counter = Math.floor(Date.now() / periodMs);
  // Include the next window's skew allowance in case the request crosses a boundary.
  const valid = new Set([-1, 0, 1, 2].map(offset => totp(secret, counter + offset)));
  let code = '000000';
  while (valid.has(code)) code = String(Number(code) + 1).padStart(6, '0');
  return code;
}

module.exports = { freshTOTP, invalidTOTP };
