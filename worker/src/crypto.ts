import { Buffer } from "node:buffer";
const enc = new TextEncoder();
export async function hash(value: string | Uint8Array): Promise<string> {
  const bytes =
    typeof value === "string" ? enc.encode(value) : new Uint8Array(value);
  return Buffer.from(await crypto.subtle.digest("SHA-256", bytes)).toString(
    "hex",
  );
}
export async function equalSecret(a: string, b: string): Promise<boolean> {
  const [x, y] = await Promise.all([hash(a), hash(b)]);
  return crypto.subtle.timingSafeEqual(enc.encode(x), enc.encode(y));
}
async function vault(admin: string): Promise<CryptoKey> {
  const bytes = await crypto.subtle.digest(
    "SHA-256",
    enc.encode("novelai-api-proxy:key-vault:v1:" + admin),
  );
  return crypto.subtle.importKey("raw", bytes, "AES-GCM", false, [
    "encrypt",
    "decrypt",
  ]);
}
export async function seal(raw: string, admin: string): Promise<string> {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const encrypted = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: nonce },
    await vault(admin),
    enc.encode(raw),
  );
  return (
    "v1:" +
    Buffer.concat([nonce, new Uint8Array(encrypted)])
      .toString("base64")
      .replace(/=+$/, "")
  );
}
export async function open(cipher: string, admin: string): Promise<string> {
  if (!cipher.startsWith("v1:")) throw new Error("invalid ciphertext");
  const bytes = Buffer.from(cipher.slice(3), "base64");
  const plain = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: bytes.subarray(0, 12) },
    await vault(admin),
    bytes.subarray(12),
  );
  return new TextDecoder().decode(plain);
}
export function randomKey(): string {
  const alphabet =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let result = "pst-";
  while (result.length < 28) {
    for (const b of crypto.getRandomValues(new Uint8Array(32))) {
      if (b < 248 && result.length < 28) result += alphabet[b % 62];
    }
  }
  return result;
}
