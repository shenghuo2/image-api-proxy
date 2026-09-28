import { env } from "cloudflare:workers";
import {
  runDurableObjectAlarm,
  runInDurableObject,
  reset,
  abortAllDurableObjects,
} from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { reserve } from "../src/ledger";
import {
  BODY_LIMIT,
  CACHE_LIMIT,
  MiB,
  RESULT_LIMIT,
  type Job,
} from "../src/model";
import type { ProxyCoordinator } from "../src/proxy";
const ADMIN = "test-admin-key-not-a-real-secret-1234567890";
const TOKEN = "test-upstream-token-at-least-16";
const generation = {
  model: "nai-diffusion-5-full",
  parameters: { width: 1024, height: 1024, steps: 28, n_samples: 1 },
};
let stub: DurableObjectStub<ProxyCoordinator>;
let generated = 0;
let upstreamStatus = 200;
let upstreamBytes: Uint8Array;
function req(
  path: string,
  method = "GET",
  body?: unknown,
  key = ADMIN,
  extra: Record<string, string> = {},
) {
  return stub.fetch(
    new Request("https://proxy.test" + path, {
      method,
      headers: {
        Authorization: "Bearer " + key,
        "Content-Type": "application/json",
        ...extra,
      },
      body:
        body === undefined
          ? undefined
          : typeof body === "string"
            ? body
            : JSON.stringify(body),
    }),
  );
}
async function setup(policy: Record<string, unknown> = {}) {
  const existing = await (await req("/admin/accounts")).json<unknown[]>();
  if (!existing.length) {
    const account = await req("/admin/accounts", "POST", {
      name: "test",
      token: TOKEN,
    });
    expect(account.status, await account.clone().text()).toBe(201);
  }
  const response = await req("/admin/keys", "POST", {
    name: "client",
    allow_opus: true,
    opus_limit_images: -1,
    ...policy,
  });
  expect(response.status, await response.clone().text()).toBe(201);
  return response.json<{ key: string; client: { id: string } }>();
}
async function finish(id: string, key: string) {
  await runDurableObjectAlarm(stub);
  const response = await req("/jobs/" + id, "GET", undefined, key);
  return response.json<{
    state: string;
    error?: string;
    expires_at?: string;
    result_expired: boolean;
  }>();
}
beforeEach(() => {
  stub = env.PROXY.getByName(crypto.randomUUID());
  generated = 0;
  upstreamStatus = 200;
  upstreamBytes = new Uint8Array([80, 75, 3, 4, 1, 2, 3]);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    if (url.endsWith("/user/subscription"))
      return Response.json({
        active: true,
        isGracePeriod: false,
        tier: 3,
        trainingStepsLeft: {
          fixedTrainingStepsLeft: 100000,
          purchasedTrainingSteps: 100000,
        },
        usage: { percent: 100, isNegative: false },
      });
    expect(url).toMatch(/^https:\/\/image.novelai.net\/ai\//);
    expect(new Headers(init?.headers).get("Authorization")).toBe(
      "Bearer " + TOKEN,
    );
    generated++;
    return new Response(upstreamBytes, {
      status: upstreamStatus,
      headers: { "Content-Type": "application/zip" },
    });
  });
});
afterEach(async () => {
  vi.restoreAllMocks();
  await reset();
});
describe("Worker managed proxy", () => {
  it("creates accounts and keys; synchronous generation preserves bytes and settles Opus", async () => {
    const client = await setup();
    const response = await req(
      "/ai/generate-image",
      "POST",
      generation,
      client.key,
    );
    expect(response.status).toBe(200);
    expect(new Uint8Array(await response.arrayBuffer())).toEqual(upstreamBytes);
    const quota = await (
      await req("/quota", "GET", undefined, client.key)
    ).json<Record<string, number>>();
    expect(quota.opus_used_images).toBe(1);
    expect(quota.opus_pending_images).toBe(0);
    expect(generated).toBe(1);
  });
  it("persists job bytes, deduplicates submissions and isolates owners", async () => {
    const client = await setup();
    const other = await setup({ name: "other" });
    const response = await req(
      "/jobs/ai/generate-image",
      "POST",
      generation,
      client.key,
      { "Idempotency-Key": "same" },
    );
    expect(response.status).toBe(202);
    const job = await response.json<{ id: string }>();
    const duplicate = await (
      await req("/jobs/ai/generate-image", "POST", generation, client.key, {
        "Idempotency-Key": "same",
      })
    ).json<{ id: string }>();
    expect(duplicate.id).toBe(job.id);
    expect(
      (await req("/jobs/" + job.id, "GET", undefined, other.key)).status,
    ).toBe(404);
    expect((await finish(job.id, client.key)).state).toBe("done");
    const result = await req(
      "/jobs/" + job.id + "/result",
      "GET",
      undefined,
      client.key,
    );
    expect(new Uint8Array(await result.arrayBuffer())).toEqual(upstreamBytes);
    expect(generated).toBe(1);
  });
});

describe("failure, admission and recovery", () => {
  it("refunds 4xx responses and leaves 5xx reservations pending", async () => {
    const client = await setup();
    upstreamStatus = 400;
    await (
      await req("/ai/generate-image", "POST", generation, client.key)
    ).arrayBuffer();
    let q = await (
      await req("/quota", "GET", undefined, client.key)
    ).json<Record<string, number>>();
    expect(q.opus_used_images).toBe(0);
    expect(q.opus_pending_images).toBe(0);
    upstreamStatus = 502;
    await (
      await req("/ai/generate-image", "POST", generation, client.key)
    ).arrayBuffer();
    q = await (
      await req("/quota", "GET", undefined, client.key)
    ).json<Record<string, number>>();
    expect(q.opus_used_images).toBe(1);
    expect(q.opus_pending_images).toBe(1);
    const reconciled = await req(
      `/admin/keys/${client.client.id}/reconcile`,
      "POST",
      {
        charged_anlas: 0,
        opus_charged_images: 0,
        opus_charged_by_account: { default: 0 },
      },
    );
    expect(reconciled.status).toBe(200);
    expect(
      (await reconciled.json<Record<string, number>>()).opus_pending_images,
    ).toBe(0);
  });
  it("rejects changed content for an existing idempotency key", async () => {
    const client = await setup();
    const first = await req(
      "/jobs/ai/generate-image",
      "POST",
      generation,
      client.key,
      { "Idempotency-Key": "conflict" },
    );
    expect(first.status).toBe(202);
    const response = await req(
      "/jobs/ai/generate-image",
      "POST",
      { ...generation, input: "different" },
      client.key,
      { "Idempotency-Key": "conflict" },
    );
    expect(response.status).toBe(409);
  });
  it("expires results after 30 minutes but does not repeat an idempotent job", async () => {
    const client = await setup();
    const j = await (
      await req("/jobs/ai/generate-image", "POST", generation, client.key, {
        "Idempotency-Key": "expiry",
      })
    ).json<{ id: string }>();
    expect((await finish(j.id, client.key)).state).toBe("done");
    await runInDurableObject(stub, (instance) => {
      const job = instance.db.get(j.id)!;
      job.expires = Date.now() - 1;
      instance.db.save(job);
    });
    expect(
      (await req(`/jobs/${j.id}/result`, "GET", undefined, client.key)).status,
    ).toBe(410);
    await runInDurableObject(stub, (instance) =>
      instance.db.cleanup(Date.now()),
    );
    const same = await (
      await req("/jobs/ai/generate-image", "POST", generation, client.key, {
        "Idempotency-Key": "expiry",
      })
    ).json<{ id: string; result_expired: boolean }>();
    expect(same.id).toBe(j.id);
    expect(same.result_expired).toBe(true);
    expect(generated).toBe(1);
    expect(
      await runInDurableObject(stub, (instance) =>
        instance.db.chunk(j.id, "result", 0),
      ),
    ).toBeUndefined();
  });
  it("deletion retains a tombstone and cannot replay the generation", async () => {
    const client = await setup(),
      j = await (
        await req("/jobs/ai/generate-image", "POST", generation, client.key, {
          "Idempotency-Key": "delete",
        })
      ).json<{ id: string }>();
    await finish(j.id, client.key);
    expect(
      (await req("/jobs/" + j.id, "DELETE", undefined, client.key)).status,
    ).toBe(204);
    const same = await (
      await req("/jobs/ai/generate-image", "POST", generation, client.key, {
        "Idempotency-Key": "delete",
      })
    ).json<{ id: string; state: string }>();
    expect(same.id).toBe(j.id);
    expect(same.state).toBe("canceled");
    expect(generated).toBe(1);
  });
  it("rejects oversized requests before contacting NovelAI", async () => {
    const client = await setup();
    const response = await req(
      "/jobs/ai/generate-image",
      "POST",
      "x".repeat(16 * 1024 * 1024 + 1),
      client.key,
    );
    expect(response.status).toBe(413);
    expect(generated).toBe(0);
  });
  it("interrupts oversized results without exposing partial bytes or refunding unknown charges", async () => {
    const client = await setup();
    upstreamBytes = new Uint8Array(32 * 1024 * 1024 + 1);
    const j = await (
      await req("/jobs/ai/generate-image", "POST", generation, client.key)
    ).json<{ id: string }>();
    const status = await finish(j.id, client.key);
    expect(status.state).toBe("interrupted");
    expect(status.error).toContain("32 MiB");
    expect(
      (await req(`/jobs/${j.id}/result`, "GET", undefined, client.key)).status,
    ).toBe(409);
    expect(
      await runInDurableObject(stub, (instance) =>
        instance.db.chunk(j.id, "result", 0),
      ),
    ).toBeUndefined();
    expect(
      (
        await (
          await req("/quota", "GET", undefined, client.key)
        ).json<Record<string, number>>()
      ).opus_pending_images,
    ).toBe(1);
  });
  it("preserves Content-Type and multipart bytes through the synchronous route", async () => {
    const client = await setup(),
      form = new FormData();
    form.append("request", JSON.stringify(generation));
    form.append("image", new Blob(["binary payload"]), "image.png");
    const source = new Response(form),
      bytes = new Uint8Array(await source.arrayBuffer()),
      type = source.headers.get("Content-Type")!;
    const original = vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      if (String(input).endsWith("/ai/generate-image")) {
        expect(new Headers(init?.headers).get("Content-Type")).toBe(type);
        expect(
          new Uint8Array(await new Response(init?.body).arrayBuffer()),
        ).toEqual(bytes);
      }
      return original(input, init);
    });
    const response = await stub.fetch(
      new Request("https://proxy.test/image/ai/generate-image", {
        method: "POST",
        headers: {
          Authorization: "Bearer " + client.key,
          "Content-Type": type,
        },
        body: bytes,
      }),
    );
    expect(response.status).toBe(200);
    await response.arrayBuffer();
  });
  it("keeps the execution slot until a synchronous response finishes", async () => {
    const client = await setup();
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const original = vi.mocked(fetch).getMockImplementation()!;
    let calls = 0;
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      if (String(input).endsWith("/ai/generate-image") && calls++ === 0)
        return new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              controller = c;
              c.enqueue(new Uint8Array([1, 2]));
            },
          }),
        );
      return original(input, init);
    });
    const first = await req(
      "/ai/generate-image",
      "POST",
      generation,
      client.key,
    );
    const second = req("/ai/generate-image", "POST", generation, client.key);
    for (let i = 0; i < 10; i++) {
      const q = await (
        await req("/admin/queue")
      ).json<{ waiting: unknown[] }>();
      if (q.waiting.length) break;
    }
    expect(calls).toBe(1);
    await runInDurableObject(stub, () => controller.close());
    await first.arrayBuffer();
    const next = await second;
    expect(next.status).toBe(200);
    await next.arrayBuffer();
    expect(calls).toBe(2);
  });
  it("enforces per-key zero-wait policy while another response owns the slot", async () => {
    const client = await setup({ queue_limit: 0 });
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    vi.mocked(fetch).mockImplementationOnce(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              controller = c;
              c.enqueue(new Uint8Array([1]));
            },
          }),
        ),
    );
    const first = await req(
      "/ai/generate-image",
      "POST",
      generation,
      client.key,
    );
    expect(
      (await req("/jobs/ai/generate-image", "POST", generation, client.key))
        .status,
    ).toBe(429);
    await runInDurableObject(stub, () => controller.close());
    await first.arrayBuffer();
  });
  it("does not reveal account secrets and invalidates a rotated client token", async () => {
    const client = await setup(),
      accounts = await req("/admin/accounts");
    expect(await accounts.text()).not.toContain(TOKEN);
    const rotated = await req(`/admin/keys/${client.client.id}/rotate`, "POST");
    expect(rotated.status).toBe(200);
    const next = await rotated.json<{ key: string }>();
    expect((await req("/quota", "GET", undefined, client.key)).status).toBe(
      401,
    );
    expect((await req("/quota", "GET", undefined, next.key)).status).toBe(200);
  });
  it("removes gallery APIs and rejects archive settings", async () => {
    expect((await req("/admin/images")).status).toBe(404);
    expect(
      (await req("/admin/settings", "PUT", { archive_enabled: true })).status,
    ).toBe(400);
    expect(
      (await req("/admin/settings", "PUT", { admin_ui_path: "/admin/no" }))
        .status,
    ).toBe(400);
    expect(
      (
        await req("/admin/settings", "PUT", {
          admin_ui_path: "/private/console",
        })
      ).status,
    ).toBe(200);
    expect(await stub.uiPath()).toBe("/private/console");
  });
});

describe("persistent scheduler boundaries", () => {
  async function seedJob(
    client: { key: string; client: { id: string } },
    running: boolean,
  ): Promise<string> {
    const id = crypto.randomUUID(),
      body = new TextEncoder().encode(JSON.stringify(generation));
    await runInDurableObject(stub, async (instance, state) => {
      const l = instance.db.ledger(),
        k = l.keys.find((k) => k.id === client.client.id)!;
      const job: Job = {
        id,
        key: k.id,
        hash: k.hash,
        route: "/ai/generate-image",
        contentType: "application/json",
        state: running ? "running" : "waiting",
        created: Date.now(),
        started: running ? Date.now() : 0,
        finished: 0,
        expires: 0,
        durable: true,
        idem: id,
        digest: "test",
        cost: { full: 50, extras: 0, opus: true, v5: true, samples: 1 },
        bodyBytes: body.length,
        resultBytes: 0,
        reserved: body.length + RESULT_LIMIT,
        status: 0,
        responseType: "",
        error: "",
      };
      if (running) job.hold = reserve(l, k, l.accounts[0], job.cost);
      instance.db.tx(() => {
        instance.db.saveLedger(l);
        instance.db.add(job);
        instance.db.putBody(id, body);
      });
      await state.storage.setAlarm(Date.now() + 60_000);
    });
    return id;
  }
  it("recovers a waiting job after eviction, but never resends a running one", async () => {
    const client = await setup();
    const running = await seedJob(client, true),
      waiting = await seedJob(client, false);
    const objectId = stub.id;
    await abortAllDurableObjects();
    stub = env.PROXY.get(objectId);
    const old = await (
      await req("/jobs/" + running, "GET", undefined, client.key)
    ).json<{ state: string }>();
    expect(old.state).toBe("interrupted");
    expect((await finish(waiting, client.key)).state).toBe("done");
    expect(generated).toBe(1);
    await runDurableObjectAlarm(stub);
    expect(generated).toBe(1);
    const q = await (
      await req("/quota", "GET", undefined, client.key)
    ).json<Record<string, number>>();
    expect(q.opus_pending_images).toBe(1);
    expect(q.opus_used_images).toBe(2);
  });
  it("reserves result capacity atomically and preserves unexpired results on admission failure", async () => {
    const client = await setup();
    const id = await seedJob(client, false);
    await runInDurableObject(stub, (instance) => {
      const job = instance.db.get(id)!;
      job.state = "done";
      job.finished = Date.now();
      job.expires = Date.now() + 1800_000;
      job.reserved = CACHE_LIMIT - RESULT_LIMIT + 1;
      instance.db.putChunk(id, "result", 0, new Uint8Array([1, 2, 3]));
      instance.db.save(job);
    });
    const result = await req(
      "/jobs/ai/generate-image",
      "POST",
      generation,
      client.key,
    );
    expect(result.status).toBe(429);
    expect(result.headers.get("Retry-After")).toBe("15");
    expect(
      await runInDurableObject(stub, (instance) =>
        instance.db.chunk(id, "result", 0),
      ),
    ).toEqual(new Uint8Array([1, 2, 3]));
  });
  it("can cancel a waiting task without generating it and releases reserved capacity", async () => {
    const client = await setup(),
      id = await seedJob(client, false);
    expect(
      (await req("/jobs/" + id, "DELETE", undefined, client.key)).status,
    ).toBe(204);
    await runDurableObjectAlarm(stub);
    expect(generated).toBe(0);
    expect(
      await runInDurableObject(stub, (instance) => instance.db.reserved()),
    ).toBe(0);
  });
  it("interrupted synchronous delivery retains its reservation", async () => {
    const client = await setup();
    vi.mocked(fetch).mockImplementationOnce(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              c.enqueue(new Uint8Array([1, 2, 3]));
            },
            pull() {
              /* Client cancellation is the only completion. */
            },
          }),
        ),
    );
    const response = await req(
      "/ai/generate-image",
      "POST",
      generation,
      client.key,
    );
    await response.body!.cancel();
    const q = await (
      await req("/quota", "GET", undefined, client.key)
    ).json<Record<string, number>>();
    expect(q.opus_pending_images).toBe(1);
  });
});

it("round-robins the pool, skips disabled accounts, and keeps fixed bindings", async () => {
  const client = await setup();
  const secondToken = "another-test-upstream-token";
  const added = await req("/admin/accounts", "POST", {
    name: "second",
    token: secondToken,
  });
  const second = await added.json<{ id: string }>();
  const original = vi.mocked(fetch).getMockImplementation()!;
  const used: string[] = [];
  vi.mocked(fetch).mockImplementation(async (input, init) => {
    if (String(input).endsWith("/user/subscription"))
      return original(input, init);
    used.push(new Headers(init?.headers).get("Authorization")!);
    return new Response(upstreamBytes);
  });
  for (let i = 0; i < 3; i++)
    await (
      await req("/ai/generate-image", "POST", generation, client.key)
    ).arrayBuffer();
  expect(used).toEqual([
    "Bearer " + TOKEN,
    "Bearer " + secondToken,
    "Bearer " + TOKEN,
  ]);
  await req("/admin/accounts/" + second.id, "PUT", { enabled: false });
  await (
    await req("/ai/generate-image", "POST", generation, client.key)
  ).arrayBuffer();
  expect(used.at(-1)).toBe("Bearer " + TOKEN);
  expect(
    (
      await req("/admin/keys/" + client.client.id, "PUT", {
        account_id: "default",
      })
    ).status,
  ).toBe(409);
  await req("/admin/accounts/" + second.id, "PUT", { enabled: true });
  const fixed = await setup({ account_id: second.id });
  await (
    await req("/ai/generate-image", "POST", generation, fixed.key)
  ).arrayBuffer();
  expect(used.at(-1)).toBe("Bearer " + secondToken);
});

it("retains Opus pending attribution when re-entering an unchanged upstream token", async () => {
  const client = await setup();
  upstreamStatus = 502;
  await (
    await req("/ai/generate-image", "POST", generation, client.key)
  ).arrayBuffer();
  expect(
    (await req("/admin/accounts/default", "PUT", { token: TOKEN })).status,
  ).toBe(200);
  const quota = await (
    await req("/quota", "GET", undefined, client.key)
  ).json<{ opus_pending_by_account: Record<string, number> }>();
  expect(quota.opus_pending_by_account.default).toBe(1);
});
