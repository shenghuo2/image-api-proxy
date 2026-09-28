import {
  assert,
  bucket,
  candidates,
  display,
  FULL,
  PERCENT,
  UNIT,
  remaining,
  type Account,
  type Cost,
  type Hold,
  type Key,
  type Ledger,
  type Quota,
} from "./model";
export function share(l: Ledger, k: Key, id: string): number {
  if (
    k.revoked ||
    !k.allow_opus ||
    k.opus_limit_mode !== "percent" ||
    ![id, "pool"].includes(k.account_id)
  )
    return 0;
  let fixed = 0,
    pooled = 0;
  for (const c of l.keys)
    if (!c.revoked && c.allow_opus && c.opus_limit_mode === "percent") {
      if (c.account_id === id) fixed += c.opus_limit_percent;
      else if (c.account_id === "pool") pooled += c.opus_limit_percent;
    }
  return k.account_id === id
    ? k.opus_limit_percent / Math.max(100, fixed)
    : (Math.max(0, 1 - Math.min(1, fixed / 100)) * k.opus_limit_percent) /
        Math.max(100, pooled);
}
export const capacity = (l: Ledger, k: Key, id: string) =>
  Math.floor(FULL * share(l, k, id) + 0.0001);
export const allocated = (l: Ledger, id: string) =>
  l.keys.reduce(
    (s, k) =>
      s +
      (!k.revoked && k.allow_opus && k.opus_limit_mode === "percent"
        ? Math.max(0, k.buckets[id]?.balance ?? 0)
        : 0),
    0,
  );
function distribute(
  l: Ledger,
  id: string,
  amount: number,
  predicted: boolean,
): void {
  for (const k of l.keys) {
    const b = bucket(k, id),
      fraction = share(l, k, id);
    if (!b.seeded || fraction <= 0) continue;
    const credit = Math.min(
      Math.floor(amount * fraction + 0.0001),
      Math.max(0, capacity(l, k, id) - b.balance),
    );
    b.balance += credit;
    if (predicted) b.predictedCredit += credit;
  }
}
function reduce(l: Ledger, id: string, projected: number, loss: number): void {
  loss = Math.max(0, loss - Math.max(0, projected - allocated(l, id)));
  while (loss > 0) {
    const total = allocated(l, id);
    if (!total) return;
    let rest = loss;
    for (const k of l.keys) {
      const b = k.buckets[id];
      if (
        !b ||
        b.balance <= 0 ||
        k.revoked ||
        !k.allow_opus ||
        k.opus_limit_mode !== "percent"
      )
        continue;
      // Use BigInt for the product, matching Go's integer division without losing precision.
      const proportional = Number(
        (BigInt(loss) * BigInt(b.balance)) / BigInt(total),
      );
      const debit = Math.min(Math.max(1, proportional), b.balance, rest);
      b.balance -= debit;
      rest -= debit;
      if (!rest) return;
    }
    loss = rest;
  }
}
export function rebalance(l: Ledger): void {
  for (const a of candidates(l)) {
    if (!a.opus) continue;
    for (const k of l.keys)
      if (k.buckets[a.id]?.seeded)
        k.buckets[a.id].balance = Math.min(
          k.buckets[a.id].balance,
          capacity(l, k, a.id),
        );
    for (const k of l.keys) {
      if (share(l, k, a.id) <= 0) continue;
      const b = bucket(k, a.id);
      if (b.seeded) continue;
      b.balance = Math.min(
        capacity(l, k, a.id),
        Math.floor(a.opus.projected * share(l, k, a.id) + 0.0001),
        Math.max(0, a.opus.projected - allocated(l, a.id)),
      );
      b.seeded = true;
    }
    const excess = allocated(l, a.id) - a.opus.projected;
    if (excess > 0) reduce(l, a.id, allocated(l, a.id), excess);
  }
}
export function topUp(l: Ledger, before: Ledger, k: Key): void {
  const previous = before.keys.find((x) => x.id === k.id);
  if (
    !previous?.allow_opus ||
    previous.opus_limit_mode !== "percent" ||
    !k.allow_opus ||
    k.opus_limit_mode !== "percent"
  )
    return;
  for (const a of candidates(l)) {
    if (!a.opus) continue;
    const increase = share(l, k, a.id) - share(before, previous, a.id),
      b = bucket(k, a.id);
    if (increase <= 0 || !b.seeded) continue;
    b.balance += Math.min(
      Math.floor(a.opus.projected * increase + 0.0001),
      Math.max(0, capacity(l, k, a.id) - b.balance),
      Math.max(0, a.opus.projected - allocated(l, a.id)),
    );
  }
}
export function syncOpus(
  l: Ledger,
  a: Account,
  q: Quota,
  nextSeconds: number,
  now: number,
): void {
  if (!q.known) {
    a.opus = undefined;
    return;
  }
  const confirmed = Math.floor(
    (Math.max(0, Math.min(100, q.percent)) * FULL) / 100 + 0.0001,
  );
  if (!a.opus) {
    a.opus = {
      confirmed,
      projected: confirmed,
      confirmedAt: now,
      nextAt: 0,
      predicted: false,
      predictedUnits: 0,
    };
    for (const k of l.keys) delete k.buckets[a.id];
  } else {
    const s = a.opus;
    if (s.predicted) {
      s.projected -= s.predictedUnits;
      for (const k of l.keys) {
        const b = k.buckets[a.id];
        if (b) {
          b.balance -= b.predictedCredit;
          b.predictedCredit = 0;
        }
      }
    }
    const delta = confirmed - s.projected;
    if (delta >= PERCENT)
      distribute(l, a.id, Math.floor(delta / PERCENT) * PERCENT, false);
    else if (delta < 0) reduce(l, a.id, s.projected, -delta);
    s.confirmed = confirmed;
    s.projected =
      delta > 0 && delta < PERCENT
        ? Math.min(s.projected, confirmed)
        : confirmed;
    s.predicted = false;
    s.predictedUnits = 0;
    s.confirmedAt = now;
  }
  a.opus.nextAt =
    nextSeconds > 0 &&
    nextSeconds <= 7 * 86400 &&
    q.percent < 100 &&
    (q.active || q.grace) &&
    q.tier === 3
      ? now + nextSeconds * 1000
      : 0;
  rebalance(l);
}
export function predict(l: Ledger, now: number): boolean {
  let changed = false;
  for (const a of candidates(l)) {
    const s = a.opus;
    if (!s || s.predicted || !s.nextAt || now < s.nextAt) continue;
    s.predictedUnits = Math.min(PERCENT, FULL - s.projected);
    s.projected += s.predictedUnits;
    distribute(l, a.id, s.predictedUnits, true);
    s.predicted = true;
    changed = true;
  }
  return changed;
}
export function percent(a: Account): number {
  const q = a.quota;
  return q && q.tier === 3 && (q.active || q.grace)
    ? Math.max(
        0,
        ((a.opus?.projected ?? Math.floor((q.percent * FULL) / 100)) * 100) /
          FULL,
      )
    : 0;
}
export function reserve(l: Ledger, k: Key, a: Account, c: Cost): Hold {
  const q = a.quota!;
  const hold: Hold = {
    fixed: 0,
    purchased: 0,
    opus: 0,
    account: a.id,
    key: k.id,
  };
  assert(
    !k.revoked &&
      (c.samples <= 1 || (k.allow_multi_image && l.settings.allow_multi_image)),
    "multi-image unavailable for key",
    402,
  );
  let paid = c.full;
  if (c.opus && q.tier === 3 && (q.active || q.grace)) {
    let free = !c.v5;
    if (c.v5) {
      assert(q.known, "Opus allowance unavailable", 402);
      if (!q.negative) {
        assert(
          percent(a) > 5,
          "Opus allowance too low to classify safely",
          402,
        );
        free = true;
      }
    }
    if (free) {
      assert(remaining(k, "opus") >= 1, "Opus quota unavailable for key", 402);
      if (k.opus_limit_mode === "percent")
        assert(
          q.known && a.opus && (k.buckets[a.id]?.balance ?? 0) >= UNIT,
          "Opus quota unavailable on account",
          402,
        );
      else if (a.opus)
        assert(
          a.opus.projected - allocated(l, a.id) >= UNIT,
          "unallocated Opus quota unavailable",
          402,
        );
      if (a.opus)
        assert(
          a.opus.projected >= UNIT,
          "upstream Opus quota unavailable",
          402,
        );
      hold.opus = 1;
      paid = c.extras;
    }
  }
  hold.fixed = Math.min(paid, remaining(k, "fixed"), q.fixed);
  paid -= hold.fixed;
  hold.purchased = Math.min(paid, remaining(k, "purchased"), q.purchased);
  paid -= hold.purchased;
  assert(paid === 0, "Anlas quota or permission insufficient", 402);
  k.fixed_anlas_spent += hold.fixed;
  k.fixed_anlas_pending += hold.fixed;
  k.purchased_anlas_spent += hold.purchased;
  k.purchased_anlas_pending += hold.purchased;
  k.opus_used_images += hold.opus;
  k.opus_pending_images += hold.opus;
  q.fixed -= hold.fixed;
  q.purchased -= hold.purchased;
  if (hold.opus && a.opus) {
    const b = bucket(k, a.id);
    b.pending += hold.opus;
    if (k.opus_limit_mode === "percent") b.balance -= UNIT;
    a.opus.projected -= UNIT;
  }
  return hold;
}
export function settle(l: Ledger, h: Hold, refund: boolean): void {
  const k = l.keys.find((k) => k.id === h.key)!,
    a = l.accounts.find((a) => a.id === h.account)!;
  k.fixed_anlas_pending -= h.fixed;
  k.purchased_anlas_pending -= h.purchased;
  k.opus_pending_images -= h.opus;
  const b = k.buckets[a.id];
  if (b) b.pending = Math.max(0, b.pending - h.opus);
  if (refund) {
    k.fixed_anlas_spent -= h.fixed;
    k.purchased_anlas_spent -= h.purchased;
    k.opus_used_images -= h.opus;
    if (a.quota) {
      a.quota.fixed += h.fixed;
      a.quota.purchased += h.purchased;
    }
    if (a.opus)
      a.opus.projected = Math.min(FULL, a.opus.projected + h.opus * UNIT);
    if (b && k.opus_limit_mode === "percent")
      b.balance = Math.min(capacity(l, k, a.id), b.balance + h.opus * UNIT);
  }
}
export function allocation(
  l: Ledger,
  id: string,
  kind: "fixed" | "purchased",
): number {
  return l.keys.reduce(
    (s, k) =>
      s +
      (k.account_id === id && k[`${kind}_anlas_limit`] !== -1
        ? remaining(k, kind)
        : 0),
    0,
  );
}
export function validateAllocations(l: Ledger): void {
  for (const kind of ["fixed", "purchased"] as const) {
    let free = 0;
    for (const a of candidates(l))
      if (a.quota) {
        const used = allocation(l, a.id, kind);
        assert(
          used <= a.quota[kind],
          "not enough unallocated upstream Anlas",
          409,
        );
        free += a.quota[kind] - used;
      }
    assert(
      allocation(l, "pool", kind) <= free,
      "not enough unallocated upstream Anlas",
      409,
    );
  }
  for (const id of ["pool", ...l.accounts.map((a) => a.id)]) {
    const total = l.keys.reduce(
      (s, k) =>
        s +
        (!k.revoked &&
        k.allow_opus &&
        k.opus_limit_mode === "percent" &&
        k.account_id === id
          ? k.opus_limit_percent
          : 0),
      0,
    );
    assert(total <= 100.000001, "Opus percentages exceed 100", 409);
  }
}
export function viewKey(l: Ledger, k: Key) {
  const {
    hash: _hash,
    cipher: _cipher,
    buckets: _buckets,
    ...publicFields
  } = k;
  const accounts = candidates(l, k);
  const opus =
    k.opus_limit_mode === "percent"
      ? accounts.reduce(
          (s, a) =>
            s + Math.max(0, Math.floor((k.buckets[a.id]?.balance ?? 0) / UNIT)),
          0,
        )
      : remaining(k, "opus");
  const capacityTotal =
    k.opus_limit_mode === "percent"
      ? accounts.reduce(
          (s, a) => s + Math.floor(capacity(l, k, a.id) / UNIT),
          0,
        )
      : k.opus_limit_images;
  return {
    ...publicFields,
    fixed_anlas_remaining: display(remaining(k, "fixed")),
    purchased_anlas_remaining: display(remaining(k, "purchased")),
    opus_remaining_images: k.revoked || !k.allow_opus ? 0 : display(opus),
    opus_effective_limit_images: capacityTotal,
    opus_predicted: accounts.some((a) => a.opus?.predicted),
    opus_confirmed_at: accounts.some((a) => a.opus)
      ? new Date(
          Math.min(
            ...accounts.filter((a) => a.opus).map((a) => a.opus!.confirmedAt),
          ),
        ).toISOString()
      : undefined,
    opus_pending_by_account: Object.fromEntries(
      Object.entries(k.buckets)
        .filter(([, b]) => b.pending > 0)
        .map(([id, b]) => [id, b.pending]),
    ),
    opus_share_warning: false,
    allocated_anlas:
      k.fixed_anlas_limit < 0 || k.purchased_anlas_limit < 0
        ? -1
        : k.fixed_anlas_limit + k.purchased_anlas_limit,
    spent_anlas: k.fixed_anlas_spent + k.purchased_anlas_spent,
    pending_anlas: k.fixed_anlas_pending + k.purchased_anlas_pending,
    remaining_anlas: display(remaining(k, "fixed") + remaining(k, "purchased")),
  };
}
export function accountView(l: Ledger, a: Account) {
  const q = a.quota!;
  const fixed = allocation(l, a.id, "fixed"),
    purchased = allocation(l, a.id, "purchased");
  return {
    account_id: a.id,
    name: a.name,
    upstream_fixed_anlas: q.officialFixed,
    upstream_purchased_anlas: q.officialPurchased,
    projected_fixed_anlas: q.fixed,
    projected_purchased_anlas: q.purchased,
    allocated_fixed_anlas: fixed,
    allocated_purchased_anlas: purchased,
    unallocated_fixed_anlas: Math.max(0, q.fixed - fixed),
    unallocated_purchased_anlas: Math.max(0, q.purchased - purchased),
    projected_opus_percent: percent(a),
    allocated_opus_images: Math.floor(allocated(l, a.id) / UNIT),
    unallocated_opus_images: Math.max(
      0,
      Math.floor(((a.opus?.projected ?? 0) - allocated(l, a.id)) / UNIT),
    ),
    opus_predicted: a.opus?.predicted ?? false,
    opus_confirmed_at: a.opus ? new Date(a.opus.confirmedAt).toISOString() : "",
    opus_next_percent_at: a.opus?.nextAt
      ? new Date(a.opus.nextAt).toISOString()
      : "",
    active: q.active,
    isGracePeriod: q.grace,
    tier: q.tier,
    snapshot_age_seconds: Math.floor((Date.now() - q.refreshed) / 1000),
  };
}
export function reconcile(
  l: Ledger,
  k: Key,
  input: Record<string, unknown>,
): void {
  const charged = input.charged_anlas,
    opus = input.opus_charged_images ?? k.opus_pending_images;
  assert(
    typeof charged === "number" &&
      Number.isInteger(charged) &&
      charged >= 0 &&
      charged <= 1e9,
    "invalid charged_anlas",
  );
  assert(
    typeof opus === "number" &&
      Number.isInteger(opus) &&
      opus >= 0 &&
      opus <= 1e7,
    "invalid opus charge",
  );
  assert(
    k.fixed_anlas_pending + k.purchased_anlas_pending + k.opus_pending_images >
      0,
    "no pending reservation",
    409,
  );
  let fixed = Math.min(charged, k.fixed_anlas_pending),
    purchased = charged - fixed;
  if (!k.purchased_anlas_pending) {
    fixed += purchased;
    purchased = 0;
  }
  k.fixed_anlas_spent = Math.max(
    0,
    k.fixed_anlas_spent + fixed - k.fixed_anlas_pending,
  );
  k.purchased_anlas_spent = Math.max(
    0,
    k.purchased_anlas_spent + purchased - k.purchased_anlas_pending,
  );
  k.fixed_anlas_pending = k.purchased_anlas_pending = 0;
  const byAccount = input.opus_charged_by_account;
  assert(
    byAccount === undefined ||
      (!!byAccount &&
        typeof byAccount === "object" &&
        !Array.isArray(byAccount)),
    "invalid account reconciliation",
  );
  const mapping = byAccount as Record<string, unknown> | undefined;
  let assigned = 0;
  for (const [id, n] of Object.entries(mapping ?? {})) {
    assert(
      typeof n === "number" &&
        Number.isInteger(n) &&
        n >= 0 &&
        n <= (k.buckets[id]?.pending ?? 0),
      "invalid account charge",
    );
    assigned += n;
  }
  assert(assigned <= opus, "invalid account charge total");
  const unknownPending =
    k.opus_pending_images -
    Object.values(k.buckets).reduce((s, b) => s + b.pending, 0);
  let left = opus;
  for (const id of Object.keys(k.buckets).sort()) {
    const b = k.buckets[id],
      used = mapping ? Number(mapping[id] ?? 0) : Math.min(left, b.pending),
      refund = b.pending - used;
    left -= used;
    b.pending = 0;
    if (refund > 0) {
      if (k.opus_limit_mode === "percent")
        b.balance = Math.min(capacity(l, k, id), b.balance + refund * UNIT);
      const a = l.accounts.find((a) => a.id === id);
      if (a?.opus)
        a.opus.projected = Math.min(FULL, a.opus.projected + refund * UNIT);
    }
  }
  if (mapping)
    assert(
      left <= unknownPending,
      "unassigned Opus charge exceeds pending reservation",
    );
  k.opus_used_images = Math.max(
    0,
    k.opus_used_images + opus - k.opus_pending_images,
  );
  k.opus_pending_images = 0;
  for (const a of l.accounts) {
    if (a.quota) a.quota.refreshed = 0;
    a.lastAttempt = 0;
  }
}
