import { env } from "cloudflare:workers";
import {
  reset,
  runDurableObjectAlarm,
  runInDurableObject,
} from "cloudflare:test";
import { afterEach, expect, it, vi } from "vitest";
import { MiB } from "../src/model";
const ADMIN = "test-admin-key-not-a-real-secret-1234567890";
afterEach(async () => {
  vi.restoreAllMocks();
  await reset();
});
it("1000 mixed generations over a simulated day stay within request, SQL and storage budgets", async (context) => {
  const stub = env.PROXY.getByName("load-" + crypto.randomUUID());
  let requests = 0,
    upstream = 0,
    peakStored = 0,
    peakDB = 0,
    peakHeap = 0;
  let now = Date.now();
  vi.spyOn(Date, "now").mockImplementation(() => now);
  const zip = new Uint8Array(2 * MiB);
  zip.set([80, 75, 3, 4]);
  // A final MessagePack image frame containing a 2 MiB binary payload.
  const prefix = new Uint8Array([
    0x81, 0xa5, 105, 109, 97, 103, 101, 0xc6, 0, 32, 0, 0,
  ]);
  const framed = new Uint8Array(4 + prefix.length + 2 * MiB);
  new DataView(framed.buffer).setUint32(0, prefix.length + 2 * MiB);
  framed.set(prefix, 4);
  framed.set([137, 80, 78, 71, 13, 10, 26, 10], 4 + prefix.length);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith("/user/subscription"))
      return Response.json({
        active: true,
        tier: 3,
        trainingStepsLeft: {
          fixedTrainingStepsLeft: 1000000,
          purchasedTrainingSteps: 1000000,
        },
        usage: { percent: 100, isNegative: false },
      });
    upstream++;
    return new Response(url.endsWith("-stream") ? framed : zip, {
      headers: { "Content-Type": "application/octet-stream" },
    });
  });
  const call = (
    path: string,
    method = "GET",
    body?: unknown,
    key = ADMIN,
    headers: Record<string, string> = {},
  ) => {
    requests++;
    return stub.fetch(
      new Request("https://load.test" + path, {
        method,
        headers: {
          Authorization: "Bearer " + key,
          "Content-Type": "application/json",
          ...headers,
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      }),
    );
  };
  expect(
    (
      await call("/admin/accounts", "POST", {
        name: "load",
        token: "test-load-token-not-real",
      })
    ).status,
  ).toBe(201);
  const created = await call("/admin/keys", "POST", {
    name: "load",
    allow_opus: true,
    opus_limit_images: -1,
  });
  expect(created.status).toBe(201);
  const { key } = await created.json<{ key: string }>();
  const body = {
    model: "nai-diffusion-5-full",
    parameters: { width: 1024, height: 1024, steps: 28, n_samples: 1 },
  };
  const before = await runInDurableObject(stub, (instance) =>
    instance.db.metrics(),
  );
  for (let i = 0; i < 1000; i++) {
    now += 86_400;
    const route = i % 2 ? "/ai/generate-image-stream" : "/ai/generate-image";
    if (i % 10 === 0) {
      const response = await call(route, "POST", body, key);
      expect(response.status).toBe(200);
      await response.arrayBuffer();
    } else {
      const submitted = await call("/jobs" + route, "POST", body, key, {
        "Idempotency-Key": String(i),
      });
      expect(submitted.status).toBe(202);
      const { id } = await submitted.json<{ id: string }>();
      await runDurableObjectAlarm(stub);
      const state = await (
        await call("/jobs/" + id, "GET", undefined, key)
      ).json<{ state: string }>();
      expect(state.state).toBe("done");
      const result = await call(
        "/jobs/" + id + "/result",
        "GET",
        undefined,
        key,
      );
      expect(result.status).toBe(200);
      await result.arrayBuffer();
    }
    // Vitest retains mock return values, including Response bodies. Clear call history
    // so the harness does not retain all generated images for the whole simulated day.
    vi.mocked(fetch).mockClear();
    vi.mocked(Date.now).mockClear();
    if (i % 20 === 0) {
      const m = await runInDurableObject(stub, (instance) =>
        instance.db.metrics(),
      );
      peakStored = Math.max(peakStored, m.reserved_bytes);
      peakDB = Math.max(peakDB, m.database_bytes);
      // workerd implements this counter; report zero explicitly if unavailable in a future runtime.
      peakHeap = Math.max(peakHeap, process.memoryUsage().heapUsed);
    }
  }
  const after = await runInDurableObject(stub, (instance) =>
    instance.db.metrics(),
  );
  const quota = await (
    await call("/quota", "GET", undefined, key)
  ).json<Record<string, number>>();
  expect(upstream).toBe(1000);
  expect(quota.opus_used_images).toBe(1000);
  expect(quota.opus_pending_images).toBe(0);
  expect(requests).toBeLessThan(30000);
  expect(after.rows_written - before.rows_written).toBeLessThan(80000);
  expect(after.rows_read - before.rows_read).toBeLessThan(4_000_000);
  expect(peakStored).toBeLessThan(100 * MiB);
  expect(peakDB).toBeLessThan(512 * MiB);
  vi.mocked(Date.now).mockRestore();
  context.task.meta.loadMetrics = {
    generations: upstream,
    api_requests: requests,
    alarm_invocations: 900,
    rows_read: after.rows_read - before.rows_read,
    rows_written: after.rows_written - before.rows_written,
    peak_stored_bytes: peakStored,
    peak_database_bytes: peakDB,
    peak_heap_bytes: peakHeap,
    note: "Local workerd, simulated wall-clock. Heap includes the test harness; zero means unsupported. Excludes real edge CPU/network latency.",
  };
}, 300_000);
