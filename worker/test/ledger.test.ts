import { describe, it, expect } from "vitest";
import {
  emptyKey,
  emptyLedger,
  UNIT,
  PERCENT,
  type Ledger,
  type Key,
  type Account,
  type Quota,
  policy,
} from "../src/model";
import {
  capacity,
  rebalance,
  reserve,
  settle,
  syncOpus,
  predict,
} from "../src/ledger";
import {
  reconcile,
  share,
  topUp,
  validateAllocations,
  viewKey,
} from "../src/ledger";
import { estimate } from "../src/cost";
import { hash, open, seal } from "../src/crypto";
function fixture(percent = 100) {
  const l = emptyLedger();
  const q: Quota = {
    fixed: 1000,
    purchased: 1000,
    officialFixed: 1000,
    officialPurchased: 1000,
    active: true,
    grace: false,
    tier: 3,
    percent,
    known: true,
    negative: false,
    refreshed: 1000,
    usage: {},
  };
  const a: Account = {
    id: "a",
    name: "account",
    enabled: true,
    cipher: "",
    tokenHash: "",
    lastAttempt: 0,
    quota: q,
  };
  l.accounts.push(a);
  const k = emptyKey();
  k.name = "key";
  k.allow_opus = true;
  k.opus_limit_mode = "percent";
  k.opus_limit_percent = 33;
  l.keys.push(k);
  syncOpus(l, a, q, 0, 1000);
  return { l, a, k, q };
}
const free = { full: 20, extras: 0, opus: true, v5: true, samples: 1 };
describe("quota and Opus parity", () => {
  it("seeds a percentage bucket from currently available quota, not its full capacity", () => {
    const { l, k } = fixture(55);
    const v = viewKey(l, k);
    expect(v.opus_effective_limit_images).toBe(570);
    expect(v.opus_remaining_images).toBe(313);
  });
  it("reserves only the selected key and refunds to its account", () => {
    const { l, k, a } = fixture();
    const other = {
      ...emptyKey(),
      id: "other",
      allow_opus: true,
      opus_limit_mode: "percent" as const,
      opus_limit_percent: 30,
    };
    l.keys.push(other);
    rebalance(l);
    const before = other.buckets.a.balance,
      balance = k.buckets.a.balance;
    const h = reserve(l, k, a, free);
    expect(k.buckets.a.balance).toBe(balance - UNIT);
    expect(other.buckets.a.balance).toBe(before);
    expect(k.opus_pending_images).toBe(1);
    settle(l, h, true);
    expect(k.buckets.a.balance).toBe(balance);
    expect(k.opus_used_images).toBe(0);
    expect(k.opus_pending_images).toBe(0);
  });
  it("predicts only one percentage step and withdraws an unconfirmed prediction", () => {
    const { l, k, a, q } = fixture(55);
    const before = k.buckets.a.balance;
    syncOpus(l, a, q, 2, 1000);
    expect(predict(l, 2999)).toBe(false);
    expect(predict(l, 3000)).toBe(true);
    expect(k.buckets.a.balance).toBe(before + Math.floor(PERCENT * 0.33));
    expect(predict(l, 100000)).toBe(false);
    syncOpus(l, a, q, 2, 100000);
    expect(k.buckets.a.balance).toBe(before);
    expect(a.opus!.predicted).toBe(false);
  });
  it("preserves a local debit when a snapshot still rounds to the previous percent", () => {
    const { l, k, a, q } = fixture();
    const h = reserve(l, k, a, free);
    settle(l, h, false);
    const before = k.buckets.a.balance;
    syncOpus(l, a, q, 0, 5000);
    expect(k.buckets.a.balance).toBe(before);
    expect(a.opus!.projected).toBe(1730 * UNIT - UNIT);
  });
  it("gives fixed-account allocations precedence over pooled shares", () => {
    const { l, k, a } = fixture();
    k.account_id = "a";
    k.opus_limit_percent = 50;
    const p = {
      ...emptyKey(),
      id: "p",
      allow_opus: true,
      opus_limit_mode: "percent" as const,
      opus_limit_percent: 50,
    };
    l.keys.push(p);
    expect(share(l, k, a.id)).toBe(0.5);
    expect(share(l, p, a.id)).toBe(0.25);
    expect(capacity(l, p, a.id)).toBe(432.5 * UNIT);
    p.opus_limit_percent = 101;
    expect(() => validateAllocations(l)).toThrow("percentages");
  });
  it("reduces outside consumption from unallocated quota before allocated balances", () => {
    const { l, k, a, q } = fixture();
    const before = k.buckets.a.balance;
    syncOpus(l, a, { ...q, percent: 50 }, 0, 2000);
    expect(k.buckets.a.balance).toBe(before);
    syncOpus(l, a, { ...q, percent: 10 }, 0, 3000);
    expect(k.buckets.a.balance).toBe(173 * UNIT);
  });
  it("increasing a share tops up only the increase, bounded by capacity", () => {
    const { l, k, a } = fixture(50),
      before = structuredClone(l),
      balance = k.buckets.a.balance;
    k.opus_limit_percent = 50;
    rebalance(l);
    topUp(l, before, k);
    expect(k.buckets.a.balance).toBe(
      balance + Math.floor(a.opus!.projected * 0.17 + 0.0001),
    );
  });
  it("count-mode keys cannot consume percentage allocations", () => {
    const { l, k, a } = fixture();
    k.opus_limit_percent = 100;
    k.buckets.a.balance = 1730 * UNIT;
    const count = { ...emptyKey(), allow_opus: true, opus_limit_images: -1 };
    l.keys.push(count);
    expect(() => reserve(l, count, a, free)).toThrow("unallocated");
  });
  it("V5 with low/unknown allowance fails safely and negative allowance uses Anlas", () => {
    const { l, k, a } = fixture(5);
    expect(() => reserve(l, k, a, free)).toThrow("classify");
    a.quota!.known = false;
    expect(() => reserve(l, k, a, free)).toThrow("unavailable");
    a.quota!.known = true;
    a.quota!.negative = true;
    k.allow_fixed_anlas = true;
    k.fixed_anlas_limit = 50;
    expect(reserve(l, k, a, free)).toMatchObject({ fixed: 20, opus: 0 });
  });
  it("Opus permissions remain necessary for a free request even with an Anlas budget", () => {
    const { l, k, a } = fixture();
    k.allow_opus = false;
    k.allow_fixed_anlas = true;
    k.fixed_anlas_limit = 1000;
    expect(() => reserve(l, k, a, free)).toThrow("Opus");
  });
  it("paid reservations split fixed/purchased budgets and multi-image needs both permissions", () => {
    const { l, k, a } = fixture();
    k.allow_fixed_anlas = true;
    k.fixed_anlas_limit = 10;
    k.allow_purchased_anlas = true;
    k.purchased_anlas_limit = 30;
    const cost = { ...free, full: 30, opus: false, samples: 2 };
    expect(() => reserve(l, k, a, cost)).toThrow("multi-image");
    l.settings.allow_multi_image = true;
    k.allow_multi_image = true;
    expect(reserve(l, k, a, cost)).toMatchObject({
      fixed: 10,
      purchased: 20,
      opus: 0,
    });
  });
  it("manual reconciliation clears pending and refunds account-specific unused Opus", () => {
    const { l, k, a } = fixture();
    const before = k.buckets.a.balance;
    reserve(l, k, a, free);
    reconcile(l, k, {
      charged_anlas: 0,
      opus_charged_images: 0,
      opus_charged_by_account: { a: 0 },
    });
    expect(k.buckets.a.balance).toBe(before);
    expect(k.opus_pending_images).toBe(0);
    expect(k.opus_used_images).toBe(0);
  });
  it("revocation, disabled accounts and expired subscriptions hide available quota", () => {
    const { l, k, a } = fixture();
    a.enabled = false;
    expect(viewKey(l, k).opus_remaining_images).toBe(0);
    a.enabled = true;
    k.revoked = true;
    expect(viewKey(l, k).opus_remaining_images).toBe(0);
  });
  it("rejects unknown policy fields and invalid numeric types", () => {
    expect(() => policy(emptyKey(), { archive_enabled: true })).toThrow(
      "unsupported",
    );
    expect(() => policy(emptyKey(), { queue_limit: 1.5 })).toThrow(
      "queue_limit",
    );
    expect(() => policy(emptyKey(), { allow_opus: "true" })).toThrow(
      "allow_opus",
    );
  });
});
describe("request and crypto compatibility", () => {
  it("estimates every current generation model family and multi-image cost", async () => {
    for (const model of [
      "nai-diffusion-3",
      "nai-diffusion-4-full",
      "nai-diffusion-4-5-full",
      "nai-diffusion-5-full",
      "nai-diffusion-5-curated",
    ]) {
      const bytes = new TextEncoder().encode(
        JSON.stringify({
          model,
          parameters: { width: 1024, height: 1024, steps: 28, n_samples: 1 },
        }),
      );
      const c = await estimate("/ai/generate-image", bytes, "application/json");
      expect(c.opus).toBe(true);
      expect(c.full).toBeGreaterThan(0);
    }
  });
  it("extracts the single multipart request part and rejects duplicates", async () => {
    const form = new FormData();
    form.append(
      "request",
      JSON.stringify({
        model: "nai-diffusion-5-full",
        parameters: { width: 512, height: 512, steps: 28, n_samples: 1 },
      }),
    );
    form.append("image", new Blob([new Uint8Array([1, 2, 3])]), "image.png");
    let r = new Response(form);
    expect(
      (
        await estimate(
          "/ai/generate-image",
          new Uint8Array(await r.arrayBuffer()),
          r.headers.get("Content-Type")!,
        )
      ).samples,
    ).toBe(1);
    form.append("request", "{}");
    r = new Response(form);
    await expect(
      estimate(
        "/ai/generate-image",
        new Uint8Array(await r.arrayBuffer()),
        r.headers.get("Content-Type")!,
      ),
    ).rejects.toThrow("one request");
  });
  it("encrypts with randomized AES-GCM and fails with a changed admin secret", async () => {
    const a = await seal("private-token", "admin-key"),
      b = await seal("private-token", "admin-key");
    expect(a).not.toBe(b);
    expect(await open(a, "admin-key")).toBe("private-token");
    await expect(open(a, "changed")).rejects.toThrow();
    expect(await hash("abc")).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
  });
});
