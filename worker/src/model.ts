export const MiB = 1024 * 1024;
export const BODY_LIMIT = 16 * MiB;
export const RESULT_LIMIT = 32 * MiB;
export const CACHE_LIMIT = 512 * MiB;
export const RESULT_TTL = 30 * 60_000;
export const IDENTITY_TTL = 24 * 3600_000;
export const EXECUTION_TIMEOUT = 7 * 60_000;
export const UNIT = 1_000_000;
export const FULL = 1730 * UNIT;
export const PERCENT = FULL / 100;
export const iso = (n: number) => new Date(n).toISOString();
export class HttpError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}
export function assert(ok: unknown, message: string, status = 400): asserts ok {
  if (!ok) throw new HttpError(status, message);
}
export function object(value: unknown): Record<string, unknown> {
  assert(
    value !== null && typeof value === "object" && !Array.isArray(value),
    "expected JSON object",
  );
  return value as Record<string, unknown>;
}
export interface Bucket {
  balance: number;
  pending: number;
  predictedCredit: number;
  seeded: boolean;
}
export interface OpusState {
  confirmed: number;
  projected: number;
  confirmedAt: number;
  nextAt: number;
  predicted: boolean;
  predictedUnits: number;
}
export interface Quota {
  fixed: number;
  purchased: number;
  officialFixed: number;
  officialPurchased: number;
  active: boolean;
  grace: boolean;
  tier: number;
  percent: number;
  known: boolean;
  negative: boolean;
  refreshed: number;
  usage: Record<string, unknown>;
}
export interface Account {
  id: string;
  name: string;
  enabled: boolean;
  cipher: string;
  tokenHash: string;
  quota?: Quota;
  opus?: OpusState;
  lastAttempt: number;
}
export interface Key {
  id: string;
  name: string;
  account_id: string;
  hash: string;
  cipher: string;
  allow_fixed_anlas: boolean;
  fixed_anlas_limit: number;
  fixed_anlas_spent: number;
  fixed_anlas_pending: number;
  allow_purchased_anlas: boolean;
  purchased_anlas_limit: number;
  purchased_anlas_spent: number;
  purchased_anlas_pending: number;
  allow_opus: boolean;
  allow_multi_image: boolean;
  opus_limit_mode: "images" | "percent";
  opus_limit_percent: number;
  opus_limit_images: number;
  opus_used_images: number;
  opus_pending_images: number;
  queue_limit: number;
  revoked: boolean;
  buckets: Record<string, Bucket>;
}
export interface Settings {
  allow_multi_image: boolean;
  admin_ui_path: string;
}
export interface Ledger {
  accounts: Account[];
  keys: Key[];
  settings: Settings;
  cursor: number;
}
export interface Cost {
  full: number;
  extras: number;
  opus: boolean;
  v5: boolean;
  samples: number;
}
export interface Hold {
  fixed: number;
  purchased: number;
  opus: number;
  account: string;
  key: string;
}
export interface Job {
  id: string;
  key: string;
  hash: string;
  route: string;
  contentType: string;
  state: "waiting" | "running" | "done" | "interrupted" | "canceled";
  created: number;
  started: number;
  finished: number;
  expires: number;
  durable: boolean;
  idem: string | null;
  digest: string;
  cost: Cost;
  bodyBytes: number;
  resultBytes: number;
  reserved: number;
  status: number;
  responseType: string;
  error: string;
  hold?: Hold;
  responseComplete?: boolean;
  generated?: boolean;
}
export function emptyLedger(): Ledger {
  return {
    accounts: [],
    keys: [],
    settings: { allow_multi_image: false, admin_ui_path: "/console" },
    cursor: 0,
  };
}
export function emptyKey(): Key {
  return {
    id: crypto.randomUUID(),
    name: "",
    account_id: "pool",
    hash: "",
    cipher: "",
    allow_fixed_anlas: false,
    fixed_anlas_limit: 0,
    fixed_anlas_spent: 0,
    fixed_anlas_pending: 0,
    allow_purchased_anlas: false,
    purchased_anlas_limit: 0,
    purchased_anlas_spent: 0,
    purchased_anlas_pending: 0,
    allow_opus: false,
    allow_multi_image: false,
    opus_limit_mode: "images",
    opus_limit_percent: 0,
    opus_limit_images: 0,
    opus_used_images: 0,
    opus_pending_images: 0,
    queue_limit: -1,
    revoked: false,
    buckets: {},
  };
}
export const candidates = (l: Ledger, k?: Key) =>
  l.accounts.filter(
    (a) =>
      a.enabled && (!k || k.account_id === "pool" || k.account_id === a.id),
  );
export const bucket = (k: Key, id: string): Bucket =>
  (k.buckets[id] ??= {
    balance: 0,
    pending: 0,
    predictedCredit: 0,
    seeded: false,
  });
export function remaining(
  k: Key,
  kind: "fixed" | "purchased" | "opus",
): number {
  if (k.revoked) return 0;
  if (kind === "opus") {
    if (!k.allow_opus) return 0;
    if (k.opus_limit_mode === "percent")
      return Object.values(k.buckets).reduce(
        (s, b) => s + Math.max(0, Math.floor(b.balance / UNIT)),
        0,
      );
    return k.opus_limit_images < 0
      ? Infinity
      : Math.max(0, k.opus_limit_images - k.opus_used_images);
  }
  if (!k[`allow_${kind}_anlas`]) return 0;
  const limit = k[`${kind}_anlas_limit`];
  return limit < 0 ? Infinity : Math.max(0, limit - k[`${kind}_anlas_spent`]);
}
export const display = (v: number) => (Number.isFinite(v) ? v : -1);
export function policy(k: Key, input: Record<string, unknown>): void {
  const booleans = [
    "allow_fixed_anlas",
    "allow_purchased_anlas",
    "allow_opus",
    "allow_multi_image",
  ] as const;
  const numbers = [
    "fixed_anlas_limit",
    "purchased_anlas_limit",
    "opus_limit_images",
    "queue_limit",
  ] as const;
  const allowed = new Set<string>([
    ...booleans,
    ...numbers,
    "name",
    "account_id",
    "opus_limit_mode",
    "opus_limit_percent",
    "allocation_anlas",
  ]);
  for (const f of Object.keys(input))
    assert(allowed.has(f), `unsupported policy field: ${f}`);
  if ("allocation_anlas" in input) {
    assert(
      !("fixed_anlas_limit" in input),
      "allocation_anlas conflicts with fixed_anlas_limit",
    );
    input.fixed_anlas_limit = input.allocation_anlas;
    input.allow_fixed_anlas ??= true;
  }
  for (const f of booleans)
    if (f in input) {
      assert(typeof input[f] === "boolean", `invalid ${f}`);
      k[f] = input[f];
    }
  for (const f of numbers)
    if (f in input) {
      const v = input[f];
      const max =
        f === "queue_limit" ? 10000 : f === "opus_limit_images" ? 1e7 : 1e9;
      assert(
        typeof v === "number" && Number.isInteger(v) && v >= -1 && v <= max,
        `invalid ${f}`,
      );
      k[f] = v;
    }
  if ("opus_limit_percent" in input) {
    const v = input.opus_limit_percent;
    assert(
      typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 100,
      "invalid percent",
    );
    k.opus_limit_percent = v;
  }
  if ("opus_limit_mode" in input) {
    assert(
      input.opus_limit_mode === "images" || input.opus_limit_mode === "percent",
      "invalid Opus mode",
    );
    k.opus_limit_mode = input.opus_limit_mode;
  }
  if ("name" in input) {
    assert(
      typeof input.name === "string" &&
        input.name.trim().length > 0 &&
        input.name.length <= 80,
      "invalid name",
    );
    k.name = input.name.trim();
  }
  if ("account_id" in input) {
    assert(typeof input.account_id === "string", "invalid account");
    k.account_id = input.account_id;
  }
}
export function validUIPath(p: unknown): p is string {
  return (
    typeof p === "string" &&
    p.length <= 128 &&
    /^\/[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*$/.test(p) &&
    !["admin", "ai", "image", "user", "quota", "healthz", "jobs"].includes(
      p.split("/")[1],
    )
  );
}
