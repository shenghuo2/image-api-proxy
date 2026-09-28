import { DurableObject } from "cloudflare:workers";
import { hash, open, seal, equalSecret, randomKey } from "./crypto";
import { canonical, estimate, routes } from "./cost";
import {
  accountView,
  allocation,
  capacity,
  percent,
  predict,
  rebalance,
  reconcile,
  reserve,
  settle,
  syncOpus,
  topUp,
  validateAllocations,
  viewKey,
} from "./ledger";
import {
  adminBody,
  bearer,
  errorResponse,
  fields,
  json,
  readLimited,
} from "./http";
import {
  assert,
  BODY_LIMIT,
  CACHE_LIMIT,
  candidates,
  emptyKey,
  EXECUTION_TIMEOUT,
  HttpError,
  IDENTITY_TTL,
  iso,
  MiB,
  object,
  remaining,
  RESULT_LIMIT,
  RESULT_TTL,
  validUIPath,
  type Account,
  type Job,
  type Key,
  type Ledger,
  type Quota,
  policy,
  FULL,
} from "./model";
import { StreamOutcome } from "./outcome";
import { Store } from "./store";
interface Live {
  run: (job: Job) => Promise<{ response: Response; done?: Promise<void> }>;
  resolve: (value: Response) => void;
  reject: (reason: unknown) => void;
  signal: AbortSignal;
}
const UPSTREAM = "https://image.novelai.net";
const noCost = { full: 0, extras: 0, opus: false, v5: false, samples: 0 };
export class ProxyCoordinator extends DurableObject<Env> {
  readonly db: Store;
  private live = new Map<string, Live>();
  private busy = false;
  private staging = false;
  private queueSize: number;
  private ttl: number;
  private lastCleanup = 0;
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.db = new Store(ctx);
    this.db.recover(Date.now());
    this.queueSize = Number(env.PROXY_QUEUE_SIZE);
    this.ttl = Number(env.PROXY_QUOTA_TTL_SECONDS) * 1000;
    if (
      !Number.isInteger(this.queueSize) ||
      this.queueSize < 1 ||
      this.queueSize > 10000 ||
      !Number.isFinite(this.ttl) ||
      this.ttl < 1000
    )
      throw new Error("invalid queue/quota configuration");
    ctx.blockConcurrencyWhile(async () => {
      this.cleanup();
      await this.schedule();
    });
  }
  uiPath(): string {
    return this.db.ledger().settings.admin_ui_path;
  }
  private ledger(): Ledger {
    const l = this.db.ledger();
    if (predict(l, Date.now())) this.db.saveLedger(l);
    return l;
  }
  private cleanup(): void {
    if (Date.now() - this.lastCleanup < 60_000) return;
    this.db.cleanup(Date.now());
    this.lastCleanup = Date.now();
  }
  private async schedule(): Promise<void> {
    const existing = await this.ctx.storage.getAlarm();
    this.db.alarmReads++;
    const waiting = this.db.waiting().length > 0;
    const running = this.db.running()[0];
    const expiry = this.db.nextExpiry();
    const times: number[] = [];
    if (waiting && !this.busy) times.push(Date.now() + 1);
    if (running)
      times.push(
        Math.max(
          Date.now() + 30_000,
          running.started + EXECUTION_TIMEOUT + 1000,
        ),
      );
    if (expiry) times.push(expiry);
    if (!times.length) {
      if (existing) {
        await this.ctx.storage.deleteAlarm();
        this.db.alarmWrites++;
      }
      return;
    }
    const next = Math.max(Date.now() + 100, Math.min(...times));
    if (!existing || existing > next || existing <= Date.now()) {
      await this.ctx.storage.setAlarm(next);
      this.db.alarmWrites++;
    }
  }
  async alarm(): Promise<void> {
    this.lastCleanup = 0;
    this.cleanup();
    if (this.busy) {
      await this.schedule();
      return;
    }
    await this.pump(true);
    await this.schedule();
  }
  private queueView() {
    const l = this.ledger();
    const view = (j: Job, position?: number) => ({
      id: j.durable ? j.id : undefined,
      position,
      key_id: j.key || undefined,
      key_name: l.keys.find((k) => k.id === j.key)?.name ?? "管理员",
      route: j.route,
      queued_at: iso(j.created),
      started_at: j.started ? iso(j.started) : undefined,
    });
    return {
      capacity: this.queueSize,
      active: this.db.running()[0] ? view(this.db.running()[0]) : null,
      waiting: this.db.waiting().map((j, i) => view(j, i + 1)),
    };
  }
  private admission(key?: Key): void {
    const waiting = this.db.waiting();
    const occupied =
      this.busy || this.db.running().length > 0 || waiting.length > 0;
    assert(!occupied || waiting.length < this.queueSize, "queue full", 429);
    if (key && occupied && key.queue_limit >= 0)
      assert(
        waiting.filter((j) => j.key === key.id).length < key.queue_limit,
        "key queue full",
        429,
      );
  }
  private makeJob(key: Key | undefined, route: string): Job {
    return {
      id: crypto.randomUUID(),
      key: key?.id ?? "",
      hash: key?.hash ?? "",
      route,
      contentType: "application/json",
      state: "waiting",
      created: Date.now(),
      started: 0,
      finished: 0,
      expires: 0,
      durable: false,
      idem: null,
      digest: "",
      cost: noCost,
      bodyBytes: 0,
      resultBytes: 0,
      reserved: 0,
      status: 0,
      responseType: "",
      error: "",
    };
  }
  private async enqueue(
    request: Request,
    key: Key | undefined,
    run: Live["run"],
  ): Promise<Response> {
    this.admission(key);
    assert(!request.signal.aborted, "request canceled", 499);
    const j = this.makeJob(
      key,
      request.method + " " + new URL(request.url).pathname,
    );
    let resolve!: Live["resolve"], reject!: Live["reject"];
    const result = new Promise<Response>((yes, no) => {
      resolve = yes;
      reject = no;
    });
    this.db.add(j);
    this.live.set(j.id, { run, resolve, reject, signal: request.signal });
    const abort = () => {
      const current = this.db.get(j.id);
      if (current?.state === "waiting") {
        this.db.remove(j.id);
        this.live.delete(j.id);
        reject(new HttpError(499, "request canceled"));
      }
    };
    request.signal.addEventListener("abort", abort, { once: true });
    this.ctx.waitUntil(this.pump(false).catch(() => this.schedule()));
    await this.schedule();
    try {
      return await result;
    } finally {
      request.signal.removeEventListener("abort", abort);
    }
  }
  private async pump(fromAlarm: boolean): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    try {
      while (true) {
        const j = this.db.waiting()[0];
        if (!j) break;
        const live = this.live.get(j.id);
        if (j.durable && !fromAlarm) break;
        if (!j.durable && (!live || live.signal.aborted)) {
          this.db.remove(j.id);
          this.live.delete(j.id);
          live?.reject(new HttpError(499, "request canceled"));
          continue;
        }
        j.state = "running";
        j.started = Date.now();
        this.db.save(j);
        await this.schedule();
        try {
          if (j.durable) await this.executeDurable(j);
          else if (live) {
            const output = await live.run(j);
            live.resolve(output.response);
            await output.done;
          }
        } catch (e) {
          if (j.durable) {
            // Preserve any committed reservation; the upstream outcome may be unknown.
            const current = this.db.get(j.id) ?? j;
            this.db.tx(() => {
              current.state = "interrupted";
              current.finished = Date.now();
              current.reserved = 0;
              current.error =
                e instanceof HttpError ? e.message : "execution interrupted";
              this.db.deleteChunks(j.id);
              this.db.save(current);
            });
          } else live?.reject(e);
        } finally {
          if (!j.durable) {
            this.live.delete(j.id);
            this.db.remove(j.id);
          }
        }
        // Each durable invocation handles at most one generation. Continue live waiters via their own events.
        if (j.durable) break;
      }
    } finally {
      this.busy = false;
      await this.schedule();
      const first = this.db.waiting()[0];
      if (first && !first.durable && this.live.has(first.id))
        this.ctx.waitUntil(this.pump(false).catch(() => this.schedule()));
    }
  }
  private async auth(
    request: Request,
    admin = false,
  ): Promise<Key | undefined> {
    assert(
      typeof this.env.PROXY_ADMIN_KEY === "string" &&
        this.env.PROXY_ADMIN_KEY.length >= 32,
      "administrator secret is not configured",
      503,
    );
    const raw = bearer(request);
    if (admin) {
      assert(
        await equalSecret(raw, this.env.PROXY_ADMIN_KEY),
        "unauthorized",
        401,
      );
      return;
    }
    const digest = await hash(raw);
    const key = this.ledger().keys.find((k) => !k.revoked && k.hash === digest);
    assert(key, "unauthorized", 401);
    return key;
  }
  private currentKey(key: Key): Key {
    const k = this.ledger().keys.find(
      (k) => k.id === key.id && k.hash === key.hash && !k.revoked,
    );
    assert(k, "key unavailable", 401);
    return k;
  }
  async fetch(request: Request): Promise<Response> {
    let response: Response;
    try {
      this.cleanup();
      const u = new URL(request.url),
        p = u.pathname;
      assert(
        !p.includes("%") && (!u.search || p === "/admin/usage/hours"),
        "unsupported request target",
      );
      if (p === "/healthz") {
        assert(
          ["GET", "HEAD"].includes(request.method),
          "method not allowed",
          405,
        );
        return new Response(request.method === "HEAD" ? null : "ok\n", {
          headers: { "Cache-Control": "no-store" },
        });
      }
      if (p.startsWith("/admin/")) {
        const origin = this.env.PROXY_ADMIN_ORIGIN;
        if (
          request.method === "OPTIONS" &&
          origin &&
          request.headers.get("Origin") === origin
        )
          response = new Response(null, { status: 204 });
        else {
          await this.auth(request, true);
          response = await this.admin(request, u);
        }
        if (origin && request.headers.get("Origin") === origin) {
          response.headers.set("Access-Control-Allow-Origin", origin);
          response.headers.set("Vary", "Origin");
          response.headers.set(
            "Access-Control-Allow-Methods",
            "GET, POST, PUT, DELETE",
          );
          response.headers.set(
            "Access-Control-Allow-Headers",
            "Authorization, Content-Type",
          );
          response.headers.set("Access-Control-Max-Age", "600");
        }
      } else {
        const key = (await this.auth(request))!;
        if (p.startsWith("/jobs/")) response = await this.jobs(request, key, p);
        else if (p === "/quota" && request.method === "GET")
          response = json({
            ...viewKey(this.ledger(), key),
            queue_length: this.db.waiting().length,
            key_queue_length: this.db.waiting().filter((j) => j.key === key.id)
              .length,
          });
        else if (
          canonical(p) === "/user/subscription" &&
          request.method === "GET"
        )
          response = await this.enqueue(request, key, async () => ({
            response: await this.subscription(this.currentKey(key)),
          }));
        else {
          const path = canonical(p);
          assert(routes.has(path), "not found", 404);
          assert(request.method === "POST", "method not allowed", 405);
          assert(
            Number(request.headers.get("Content-Length") ?? 0) <= BODY_LIMIT,
            "request body too large",
            413,
          );
          response = await this.enqueue(request, key, async (job) => {
            this.currentKey(key);
            const bytes = await readLimited(request.body, BODY_LIMIT);
            job.contentType =
              request.headers.get("Content-Type") ?? "application/json";
            job.route = path;
            try {
              job.cost = await estimate(path, bytes, job.contentType);
            } catch (e) {
              throw e instanceof HttpError
                ? e
                : new HttpError(400, "invalid generation request");
            }
            return this.generate(job, bytes, request.signal);
          });
        }
      }
    } catch (e) {
      response = errorResponse(e);
    }
    response.headers.set("Cache-Control", "no-store");
    if (
      new URL(request.url).pathname.startsWith("/admin/") &&
      this.env.PROXY_ADMIN_ORIGIN &&
      request.headers.get("Origin") === this.env.PROXY_ADMIN_ORIGIN
    ) {
      response.headers.set(
        "Access-Control-Allow-Origin",
        this.env.PROXY_ADMIN_ORIGIN,
      );
      response.headers.set("Vary", "Origin");
    }
    return response;
  }
  private async quota(id: string, force = false): Promise<void> {
    let l = this.ledger();
    let a = l.accounts.find((a) => a.id === id);
    assert(a?.enabled, "upstream account unavailable", 502);
    const now = Date.now(),
      floor = Math.min(30_000, this.ttl);
    if (a.quota && now - a.quota.refreshed < (force ? floor : this.ttl)) return;
    assert(now - a.lastAttempt >= floor, "quota refresh backoff", 502);
    a.lastAttempt = now;
    this.db.saveLedger(l);
    let data: Record<string, unknown>;
    try {
      const token = await open(a.cipher, this.env.PROXY_ADMIN_KEY);
      const response = await fetch(UPSTREAM + "/user/subscription", {
        headers: { Authorization: "Bearer " + token },
        redirect: "manual",
        signal: AbortSignal.timeout(25_000),
      });
      if (response.status !== 200) {
        await response.body?.cancel();
        throw new Error("quota unavailable");
      }
      data = object(
        JSON.parse(
          new TextDecoder().decode(await readLimited(response.body, MiB)),
        ),
      );
    } catch {
      throw new HttpError(502, "upstream quota unavailable");
    }
    const steps = object(data.trainingStepsLeft);
    const fixed = steps.fixedTrainingStepsLeft,
      purchased = steps.purchasedTrainingSteps;
    assert(
      typeof fixed === "number" &&
        Number.isInteger(fixed) &&
        fixed >= 0 &&
        fixed <= 1e9 &&
        typeof purchased === "number" &&
        Number.isInteger(purchased) &&
        purchased >= 0 &&
        purchased <= 1e9,
      "invalid upstream balance",
      502,
    );
    const usage =
      data.usage && typeof data.usage === "object" && !Array.isArray(data.usage)
        ? (data.usage as Record<string, unknown>)
        : {};
    const known =
      typeof usage.percent === "number" &&
      Number.isFinite(usage.percent) &&
      usage.percent >= 0 &&
      usage.percent <= 100;
    const q: Quota = {
      fixed,
      purchased,
      officialFixed: fixed,
      officialPurchased: purchased,
      active: data.active === true,
      grace: data.isGracePeriod === true,
      tier: Number(data.tier) || 0,
      percent: known ? Number(usage.percent) : 0,
      known,
      negative: usage.isNegative === true,
      refreshed: Date.now(),
      usage,
    };
    l = this.ledger();
    a = l.accounts.find((a) => a.id === id)!;
    a.quota = q;
    syncOpus(
      l,
      a,
      q,
      typeof usage.timeUntilNextPercent === "number"
        ? Math.ceil(usage.timeUntilNextPercent)
        : 0,
      Date.now(),
    );
    this.db.saveLedger(l);
  }
  private async quotas(force = false, ids?: string[]): Promise<string[]> {
    const failed: string[] = [];
    for (const a of candidates(this.ledger()).filter(
      (a) => !ids || ids.includes(a.id),
    )) {
      try {
        await this.quota(a.id, force);
      } catch {
        failed.push(a.id);
      }
    }
    return failed;
  }
  private async select(
    job: Job,
  ): Promise<{ token: string; hold: NonNullable<Job["hold"]> }> {
    let l = this.ledger();
    let k = l.keys.find(
      (k) => k.id === job.key && k.hash === job.hash && !k.revoked,
    );
    assert(k, "key unavailable", 401);
    const list = candidates(l, k);
    assert(list.length, "no enabled upstream account", 503);
    const start = k.account_id === "pool" ? l.cursor % list.length : 0;
    let failure: unknown = new HttpError(402, "quota unavailable");
    for (let offset = 0; offset < list.length; offset++) {
      const index = (start + offset) % list.length,
        id = list[index].id;
      try {
        await this.quota(id);
        l = this.ledger();
        k = l.keys.find(
          (k) => k.id === job.key && k.hash === job.hash && !k.revoked,
        );
        assert(k, "key unavailable", 401);
        const a = l.accounts.find((a) => a.id === id)!;
        const token = await open(a.cipher, this.env.PROXY_ADMIN_KEY);
        // Reload after crypto yields, so predictions made by read requests cannot be overwritten.
        l = this.ledger();
        k = l.keys.find((k) => k.id === job.key)!;
        const hold = reserve(
          l,
          k,
          l.accounts.find((a) => a.id === id)!,
          job.cost,
        );
        if (k.account_id === "pool") l.cursor = (index + 1) % list.length;
        job.hold = hold;
        this.db.tx(() => {
          this.db.saveLedger(l);
          this.db.save(job);
        });
        return { token, hold };
      } catch (e) {
        failure = e;
      }
    }
    throw failure;
  }
  private async generate(
    job: Job,
    bytes: Uint8Array,
    clientSignal?: AbortSignal,
  ): Promise<{ response: Response; done: Promise<void> }> {
    const { token, hold } = await this.select(job);
    const timeout = AbortSignal.timeout(EXECUTION_TIMEOUT);
    const signal = clientSignal
      ? AbortSignal.any([timeout, clientSignal])
      : timeout;
    const upstream = await fetch(UPSTREAM + canonical(job.route), {
      method: "POST",
      headers: {
        Authorization: "Bearer " + token,
        "Content-Type": job.contentType,
      },
      body: new Uint8Array(bytes),
      redirect: "manual",
      signal,
    });
    job.status = upstream.status;
    job.responseType =
      upstream.headers.get("Content-Type") ?? "application/octet-stream";
    const outcome =
      canonical(job.route) === "/ai/generate-image-stream"
        ? new StreamOutcome()
        : undefined;
    const reader = upstream.body?.getReader();
    let complete!: () => void;
    const done = new Promise<void>((resolve) => {
      complete = resolve;
    });
    let ended = false,
      received = 0;
    const finish = (certain: boolean) => {
      if (ended) return;
      ended = true;
      try {
        const status = upstream.status;
        job.responseComplete = certain;
        job.generated =
          certain &&
          status >= 200 &&
          status < 300 &&
          received > 0 &&
          job.cost.samples > 0 &&
          (!outcome || outcome.success());
        if (
          !job.durable &&
          certain &&
          ((status >= 200 && status < 300) || (status >= 400 && status < 500))
        ) {
          this.db.tx(() => {
            const l = this.ledger();
            settle(l, hold, status >= 400);
            this.db.saveLedger(l);
            if (
              status < 300 &&
              received > 0 &&
              job.cost.samples > 0 &&
              (!outcome || outcome.success())
            )
              this.db.record(job.cost.samples, Date.now());
          });
        }
      } finally {
        signal.removeEventListener("abort", abort);
        complete();
      }
    };
    const abort = () => {
      void reader?.cancel().catch(() => {});
      finish(false);
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
    const stream = new ReadableStream<Uint8Array>({
      pull: async (controller) => {
        if (ended) {
          controller.error(new Error("upstream interrupted"));
          return;
        }
        try {
          const next = reader
            ? await reader.read()
            : { done: true, value: undefined };
          if (next.done) {
            finish(true);
            controller.close();
          } else {
            received += next.value.length;
            outcome?.write(next.value);
            controller.enqueue(next.value);
          }
        } catch (e) {
          finish(false);
          controller.error(e);
        }
      },
      cancel: async () => {
        try {
          await reader?.cancel();
        } finally {
          finish(false);
        }
      },
    });
    const headers = new Headers({
      "Content-Type": job.responseType,
      "Cache-Control": "no-store",
    });
    // Whitelist response metadata; never relay cookies or credentials.
    for (const h of ["Content-Disposition", "Retry-After"]) {
      const v = upstream.headers.get(h);
      if (v) headers.set(h, v);
    }
    if ([204, 205, 304].includes(upstream.status)) {
      await reader?.cancel();
      finish(true);
      return {
        response: new Response(null, { status: upstream.status, headers }),
        done,
      };
    }
    return {
      response: new Response(stream, { status: upstream.status, headers }),
      done,
    };
  }
  private jobView(j: Job) {
    const expired = j.expires > 0 && Date.now() >= j.expires;
    return {
      id: j.id,
      state: j.state,
      route: j.route,
      created_at: iso(j.created),
      queued_at: iso(j.created),
      started_at: j.started ? iso(j.started) : undefined,
      finished_at: j.finished ? iso(j.finished) : undefined,
      expires_at: j.expires ? iso(j.expires) : undefined,
      result_expired: expired,
      result_url:
        j.state === "done" && !expired ? `/jobs/${j.id}/result` : undefined,
      upstream_status: j.status || undefined,
      error: j.error || undefined,
      poll_after_seconds:
        j.state === "waiting" && Date.now() - j.created > 30_000 ? 15 : 5,
    };
  }
  private async jobs(
    request: Request,
    key: Key,
    path: string,
  ): Promise<Response> {
    if (request.method === "POST" && routes.has(canonical(path.slice(5)))) {
      assert(!this.staging, "another task upload is being staged", 429);
      assert(
        Number(request.headers.get("Content-Length") ?? 0) <= BODY_LIMIT,
        "request body too large",
        413,
      );
      this.staging = true;
      try {
        const bytes = await readLimited(request.body, BODY_LIMIT),
          route = canonical(path.slice(5)),
          type = request.headers.get("Content-Type") ?? "application/json";
        const idem = request.headers.get("Idempotency-Key");
        assert(
          idem === null ||
            (idem.length > 0 &&
              idem.length <= 128 &&
              !/[\x00-\x1f\x7f]/.test(idem)),
          "invalid Idempotency-Key",
        );
        const digest = await hash(
            route + "\n" + type + "\n" + (await hash(bytes)),
          ),
          existing = idem ? this.db.byIdem(key.id, idem) : undefined;
        if (
          existing &&
          (!existing.finished || Date.now() - existing.finished < IDENTITY_TTL)
        ) {
          assert(
            existing.digest === digest,
            "idempotency key conflicts with request",
            409,
          );
          return json(this.jobView(existing), 202, {
            Location: `/jobs/${existing.id}`,
            "Retry-After": "5",
          });
        }
        if (existing) this.db.remove(existing.id);
        const j = this.makeJob(key, route);
        j.contentType = type;
        j.durable = true;
        j.idem = idem;
        j.digest = digest;
        j.bodyBytes = bytes.length;
        try {
          j.cost = await estimate(route, bytes, type);
        } catch (e) {
          throw e instanceof HttpError
            ? e
            : new HttpError(400, "invalid generation request");
        }
        j.reserved = bytes.length + RESULT_LIMIT;
        this.db.tx(() => {
          this.admission(this.currentKey(key));
          assert(
            this.db.reserved() + j.reserved <= CACHE_LIMIT,
            "temporary task storage full",
            429,
          );
          this.db.add(j);
          this.db.putBody(j.id, bytes);
        });
        await this.schedule();
        return json(this.jobView(j), 202, {
          Location: `/jobs/${j.id}`,
          "Retry-After": "5",
        });
      } finally {
        this.staging = false;
      }
    }
    const match = /^\/jobs\/([\w-]+)(\/result)?$/.exec(path);
    assert(match, "not found", 404);
    const j = this.db.get(match[1]);
    assert(j?.durable && j.key === key.id, "not found", 404);
    if (request.method === "GET" && !match[2])
      return json(this.jobView(j), 200, {
        "Retry-After": String(this.jobView(j).poll_after_seconds),
      });
    if (request.method === "GET" && match[2]) {
      assert(!j.expires || Date.now() < j.expires, "task result expired", 410);
      assert(j.state === "done", "task result unavailable", 409);
      let part = 0;
      const stream = new ReadableStream<Uint8Array>({
        pull: (controller) => {
          const current = this.db.get(j.id);
          if (
            !current ||
            current.expires <= Date.now() ||
            current.state !== "done"
          ) {
            controller.error(new Error("task result expired or deleted"));
            return;
          }
          if (part * MiB >= j.resultBytes) {
            controller.close();
            return;
          }
          const data = this.db.chunk(j.id, "result", part++);
          if (!data) controller.error(new Error("task result unavailable"));
          else controller.enqueue(data);
        },
      });
      return new Response([204, 205, 304].includes(j.status) ? null : stream, {
        status: j.status,
        headers: {
          "Content-Type": j.responseType,
          "Cache-Control": "no-store",
        },
      });
    }
    if (request.method === "DELETE" && !match[2]) {
      assert(
        j.state !== "running",
        "running task cannot be canceled safely",
        409,
      );
      this.db.tx(() => {
        this.db.deleteChunks(j.id);
        j.state = "canceled";
        j.finished ||= Date.now();
        j.reserved = 0;
        j.resultBytes = 0;
        j.expires = 0;
        this.db.save(j);
      });
      await this.schedule();
      return new Response(null, { status: 204 });
    }
    throw new HttpError(405, "method not allowed");
  }
  private async executeDurable(j: Job): Promise<void> {
    const bytes = this.db.body(j);
    let generated: Awaited<ReturnType<ProxyCoordinator["generate"]>>;
    try {
      generated = await this.generate(j, bytes);
    } catch (e) {
      if (j.hold) throw e;
      const response = errorResponse(e),
        result = new Uint8Array(await response.arrayBuffer());
      this.db.tx(() => {
        this.db.deleteChunks(j.id);
        this.db.putChunk(j.id, "result", 0, result);
        j.state = "done";
        j.status = response.status;
        j.responseType = "text/plain";
        j.finished = Date.now();
        j.expires = j.finished + RESULT_TTL;
        j.resultBytes = result.length;
        j.reserved = result.length;
        this.db.save(j);
      });
      return;
    }
    const reader = generated.response.body?.getReader();
    let buffer = new Uint8Array(MiB),
      filled = 0,
      part = 0,
      total = 0;
    try {
      if (reader)
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          total += value.length;
          assert(total <= RESULT_LIMIT, "task result exceeds 32 MiB", 502);
          let at = 0;
          while (at < value.length) {
            const n = Math.min(MiB - filled, value.length - at);
            buffer.set(value.subarray(at, at + n), filled);
            at += n;
            filled += n;
            if (filled === MiB) {
              this.db.putChunk(j.id, "result", part++, buffer);
              filled = 0;
              buffer = new Uint8Array(MiB);
            }
          }
        }
      if (filled)
        this.db.putChunk(j.id, "result", part, buffer.subarray(0, filled));
      await generated.done;
      assert(j.responseComplete, "upstream response interrupted", 502);
      this.db.tx(() => {
        this.db.deleteChunks(j.id, "body");
        if (
          (j.status >= 200 && j.status < 300) ||
          (j.status >= 400 && j.status < 500)
        ) {
          const l = this.ledger();
          settle(l, j.hold!, j.status >= 400);
          this.db.saveLedger(l);
          if (j.generated) this.db.record(j.cost.samples, Date.now());
        }
        j.state = "done";
        j.finished = Date.now();
        j.expires = j.finished + RESULT_TTL;
        j.resultBytes = total;
        j.reserved = total;
        this.db.save(j);
      });
    } catch (e) {
      await reader?.cancel().catch(() => {});
      throw e;
    }
  }
  private async subscription(key: Key): Promise<Response> {
    const ids = candidates(this.ledger(), key).map((a) => a.id),
      failed = await this.quotas(false, ids);
    const l = this.ledger(),
      k = l.keys.find((k) => k.id === key.id)!;
    const accounts = candidates(l, k).filter(
      (a) => a.quota && !failed.includes(a.id),
    );
    assert(accounts.length, "upstream quota unavailable", 502);
    const fixed = accounts.reduce((s, a) => s + a.quota!.fixed, 0),
      purchased = accounts.reduce((s, a) => s + a.quota!.purchased, 0);
    const allowed =
      k.opus_limit_mode === "percent"
        ? Number(viewKey(l, k).opus_remaining_images)
        : remaining(k, "opus");
    return json({
      active: accounts.some((a) => a.quota!.active),
      isGracePeriod: accounts.some((a) => a.quota!.grace),
      tier: Math.max(...accounts.map((a) => a.quota!.tier)),
      trainingStepsLeft: {
        fixedTrainingStepsLeft: Math.min(fixed, remaining(k, "fixed")),
        purchasedTrainingSteps: Math.min(purchased, remaining(k, "purchased")),
      },
      usage: {
        ...accounts[0].quota!.usage,
        percent: Math.max(
          0,
          Math.min(
            100,
            accounts.reduce((s, a) => s + percent(a), 0),
            (allowed * 100) / 1730,
          ),
        ),
        isNegative: accounts[0].quota!.negative,
        timeUntilNextPercent: accounts[0].opus?.nextAt
          ? Math.max(
              0,
              Math.ceil((accounts[0].opus.nextAt - Date.now()) / 1000),
            )
          : 0,
      },
    });
  }
  private async adminQuota(force: boolean): Promise<Response> {
    const failed = await this.quotas(force),
      l = this.ledger(),
      accounts = candidates(l).filter((a) => a.quota && !failed.includes(a.id));
    assert(accounts.length, "upstream quota unavailable", 502);
    const views = accounts.map((a) => accountView(l, a));
    const sum = (
      f: "officialFixed" | "officialPurchased" | "fixed" | "purchased",
    ) => accounts.reduce((s, a) => s + a.quota![f], 0);
    const fixed = accounts.reduce(
      (s, a) => s + allocation(l, a.id, "fixed"),
      allocation(l, "pool", "fixed"),
    );
    const purchased = accounts.reduce(
      (s, a) => s + allocation(l, a.id, "purchased"),
      allocation(l, "pool", "purchased"),
    );
    return json({
      upstream_fixed_anlas: sum("officialFixed"),
      upstream_purchased_anlas: sum("officialPurchased"),
      upstream_anlas: sum("officialFixed") + sum("officialPurchased"),
      projected_fixed_anlas: sum("fixed"),
      projected_purchased_anlas: sum("purchased"),
      allocated_fixed_anlas: fixed,
      allocated_purchased_anlas: purchased,
      allocated_remaining_anlas: fixed + purchased,
      unallocated_fixed_anlas: Math.max(0, sum("fixed") - fixed),
      unallocated_purchased_anlas: Math.max(0, sum("purchased") - purchased),
      unallocated_anlas:
        Math.max(0, sum("fixed") - fixed) +
        Math.max(0, sum("purchased") - purchased),
      active: accounts.some((a) => a.quota!.active),
      isGracePeriod: accounts.some((a) => a.quota!.grace),
      tier: Math.max(...accounts.map((a) => a.quota!.tier)),
      projected_opus_percent: Math.min(
        100,
        accounts.reduce((s, a) => s + percent(a), 0),
      ),
      snapshot_age_seconds: Math.max(
        ...views.map((a) => a.snapshot_age_seconds),
      ),
      queue_length: this.db.waiting().length,
      account_quotas: views,
      account_errors: failed.map((id) => ({
        account_id: id,
        name: l.accounts.find((a) => a.id === id)!.name,
      })),
      unlimited_fixed_keys: l.keys.filter(
        (k) => !k.revoked && k.allow_fixed_anlas && k.fixed_anlas_limit < 0,
      ).length,
      unlimited_purchased_keys: l.keys.filter(
        (k) =>
          !k.revoked && k.allow_purchased_anlas && k.purchased_anlas_limit < 0,
      ).length,
      unlimited_opus_keys: l.keys.filter(
        (k) =>
          !k.revoked &&
          k.allow_opus &&
          k.opus_limit_mode === "images" &&
          k.opus_limit_images < 0,
      ).length,
    });
  }
  private async publicAccount(a: Account, l: Ledger) {
    let valid = true;
    try {
      await open(a.cipher, this.env.PROXY_ADMIN_KEY);
    } catch {
      valid = false;
    }
    return {
      id: a.id,
      name: a.name,
      enabled: a.enabled,
      token_configured: valid,
      key_count: l.keys.filter((k) => k.account_id === a.id).length,
    };
  }
  private async admin(request: Request, u: URL): Promise<Response> {
    const p = u.pathname,
      method = request.method;
    if (p === "/admin/queue" && method === "GET") return json(this.queueView());
    if (p === "/admin/settings" && method === "GET")
      return json(this.ledger().settings);
    if (p === "/admin/runtime" && method === "GET")
      return json({
        ...this.db.metrics(),
        result_retention_seconds: RESULT_TTL / 1000,
        max_body_bytes: BODY_LIMIT,
        max_result_bytes: RESULT_LIMIT,
      });
    if (p === "/admin/keys" && method === "GET") {
      const l = this.ledger(),
        result = [];
      for (const k of l.keys) {
        let raw: string | undefined;
        try {
          raw = await open(k.cipher, this.env.PROXY_ADMIN_KEY);
        } catch {
          /* Rotate to reveal again. */
        }
        result.push({ ...viewKey(l, k), key: raw });
      }
      return json(result);
    }
    if (p === "/admin/accounts" && method === "GET") {
      const l = this.ledger();
      return json(
        await Promise.all(l.accounts.map((a) => this.publicAccount(a, l))),
      );
    }
    if (p === "/admin/usage/hours" && method === "GET") {
      assert(
        [...u.searchParams.keys()].every((k) =>
          ["from", "to", "offset_minutes"].includes(k),
        ),
        "invalid filter",
      );
      const from = Date.parse(u.searchParams.get("from") ?? ""),
        to = Date.parse(u.searchParams.get("to") ?? ""),
        offset = Number(u.searchParams.get("offset_minutes"));
      assert(
        Number.isFinite(from) &&
          Number.isFinite(to) &&
          to > from &&
          to - from <= 8 * 86400_000 &&
          u.searchParams.has("offset_minutes") &&
          Number.isInteger(offset) &&
          offset >= -840 &&
          offset <= 840,
        "invalid time range",
      );
      return json({
        hours: this.db.exec(
          "SELECT strftime('%Y-%m-%d',quarter-?*60,'unixepoch') date, CAST(strftime('%H',quarter-?*60,'unixepoch') AS INTEGER) hour,SUM(images) count,SUM(generations) generations FROM usage WHERE quarter>=? AND quarter<? GROUP BY 1,2 ORDER BY 1,2",
          offset,
          offset,
          from / 1000,
          to / 1000,
        ),
      });
    }
    if (p.startsWith("/admin/images")) throw new HttpError(404, "not found");
    if (
      (p === "/admin/quota" && method === "GET") ||
      (p === "/admin/quota/refresh" && method === "POST")
    )
      return this.enqueue(request, undefined, async () => ({
        response: await this.adminQuota(method === "POST"),
      }));
    const aq = /^\/admin\/accounts\/([\w-]+)\/quota(\/refresh)?$/.exec(p);
    if (aq && method === (aq[2] ? "POST" : "GET"))
      return this.enqueue(request, undefined, async () => {
        await this.quota(aq[1], !!aq[2]);
        const l = this.ledger();
        return {
          response: json(
            accountView(
              l,
              l.accounts.find((a) => a.id === aq[1])!,
            ),
          ),
        };
      });
    const accountRoute =
        p === "/admin/accounts" || /^\/admin\/accounts\/[\w-]+$/.test(p),
      keyRoute =
        p === "/admin/keys" ||
        /^\/admin\/keys\/[\w-]+(?:\/(?:rotate|reconcile))?$/.test(p);
    assert(
      p === "/admin/settings" || accountRoute || keyRoute,
      "not found",
      404,
    );
    assert(
      ["PUT", "POST", "DELETE"].includes(method),
      "method not allowed",
      405,
    );
    const input =
      method === "DELETE" || p.endsWith("/rotate")
        ? {}
        : await adminBody(request);
    return this.enqueue(request, undefined, async () => ({
      response: await this.mutateAdmin(p, method, input),
    }));
  }
  private async mutateAdmin(
    path: string,
    method: string,
    input: Record<string, unknown>,
  ): Promise<Response> {
    if (path === "/admin/settings") {
      assert(method === "PUT", "method not allowed", 405);
      fields(input, ["allow_multi_image", "admin_ui_path"]);
      assert(Object.keys(input).length, "empty settings");
      const l = this.ledger();
      if ("allow_multi_image" in input) {
        assert(
          typeof input.allow_multi_image === "boolean",
          "invalid multi-image setting",
        );
        l.settings.allow_multi_image = input.allow_multi_image;
      }
      if ("admin_ui_path" in input) {
        assert(validUIPath(input.admin_ui_path), "invalid admin UI path");
        l.settings.admin_ui_path = input.admin_ui_path;
      }
      this.db.saveLedger(l);
      return json(l.settings);
    }
    if (path.startsWith("/admin/accounts"))
      return this.mutateAccount(path, method, input);
    const pieces = path.split("/"),
      id = pieces[3],
      action = pieces[4];
    if ((method === "POST" && !id) || method === "PUT") await this.quotas();
    let l = this.ledger(),
      k = l.keys.find((k) => k.id === id);
    if (method === "POST" && !id) {
      k = emptyKey();
      policy(k, input);
      assert(k.name, "name required");
      const raw = randomKey(),
        cipher = await seal(raw, this.env.PROXY_ADMIN_KEY),
        digest = await hash(raw);
      l = this.ledger();
      k.hash = digest;
      k.cipher = cipher;
      l.keys.push(k);
      this.validateKey(l, k);
      rebalance(l);
      this.db.saveLedger(l);
      return json({ key: raw, client: viewKey(l, k) }, 201);
    }
    assert(k, "key not found", 404);
    if (method === "POST" && action === "rotate") {
      assert(!k.revoked, "key revoked", 404);
      const raw = randomKey(),
        cipher = await seal(raw, this.env.PROXY_ADMIN_KEY),
        digest = await hash(raw);
      l = this.ledger();
      k = l.keys.find((k) => k.id === id)!;
      k.hash = digest;
      k.cipher = cipher;
      this.db.saveLedger(l);
      return json({ key: raw, client: viewKey(l, k) });
    }
    if (method === "POST" && action === "reconcile") {
      fields(input, [
        "charged_anlas",
        "opus_charged_images",
        "opus_charged_by_account",
      ]);
      reconcile(l, k, input);
      this.db.saveLedger(l);
      return json(viewKey(l, k));
    }
    if (method === "DELETE" && !action) {
      k.revoked = true;
      rebalance(l);
      this.db.saveLedger(l);
      return new Response(null, { status: 204 });
    }
    if (method === "PUT" && !action) {
      assert(!k.revoked, "key revoked", 404);
      const before = structuredClone(l),
        previous = structuredClone(k);
      policy(k, input);
      assert(
        k.account_id === previous.account_id ||
          previous.fixed_anlas_spent +
            previous.purchased_anlas_spent +
            previous.opus_used_images ===
            0,
        "cannot move a key with usage",
        409,
      );
      if (
        (!previous.allow_opus && k.allow_opus) ||
        previous.opus_limit_mode !== k.opus_limit_mode ||
        k.account_id !== previous.account_id
      )
        for (const b of Object.values(k.buckets)) {
          b.balance = 0;
          b.seeded = false;
          b.predictedCredit = 0;
        }
      if (!k.allow_opus || k.opus_limit_mode !== "percent")
        for (const b of Object.values(k.buckets)) b.balance = 0;
      this.validateKey(l, k, previous);
      rebalance(l);
      topUp(l, before, k);
      this.db.saveLedger(l);
      return json(viewKey(l, k));
    }
    throw new HttpError(405, "method not allowed");
  }
  private validateKey(l: Ledger, k: Key, previous?: Key): void {
    assert(candidates(l, k).length, "account unavailable");
    assert(
      k.account_id === "pool" ||
        l.accounts.find((a) => a.id === k.account_id)?.quota,
      "upstream account quota unavailable",
      502,
    );
    assert(
      !k.allow_multi_image ||
        previous?.allow_multi_image ||
        l.settings.allow_multi_image,
      "enable multi-image in settings first",
      409,
    );
    validateAllocations(l);
  }
  private async mutateAccount(
    path: string,
    method: string,
    input: Record<string, unknown>,
  ): Promise<Response> {
    const id = path.split("/")[3];
    let l = this.ledger(),
      a = l.accounts.find((a) => a.id === id);
    if (id && method === "DELETE") {
      assert(a, "account not found", 404);
      assert(
        !l.keys.some((k) => k.account_id === id),
        "account has assigned keys",
        409,
      );
      assert(
        !l.keys.some((k) => (k.buckets[id]?.pending ?? 0) > 0),
        "account has pending reservations",
        409,
      );
      l.accounts = l.accounts.filter((a) => a.id !== id);
      for (const k of l.keys) delete k.buckets[id];
      this.db.saveLedger(l);
      return new Response(null, { status: 204 });
    }
    assert(
      (!id && method === "POST") || (id && method === "PUT"),
      "method not allowed",
      405,
    );
    if (id) assert(a, "account not found", 404);
    fields(input, ["name", "token", "enabled"]);
    if ("name" in input || !id)
      assert(
        typeof input.name === "string" &&
          input.name.trim().length > 0 &&
          input.name.length <= 80,
        "invalid account name",
      );
    if ("token" in input || !id)
      assert(
        typeof input.token === "string" &&
          input.token.length >= 16 &&
          input.token.length <= 2048,
        "invalid account token",
      );
    if ("enabled" in input)
      assert(typeof input.enabled === "boolean", "invalid enabled");
    let digest: string | undefined, cipher: string | undefined;
    if (typeof input.token === "string") {
      digest = await hash(input.token);
      cipher = await seal(input.token, this.env.PROXY_ADMIN_KEY);
      l = this.ledger();
      a = l.accounts.find((a) => a.id === id);
      assert(
        !l.accounts.some((a) => a.id !== id && a.tokenHash === digest),
        "account token already exists",
        409,
      );
    }
    if (!a) {
      a = {
        id: l.accounts.length ? crypto.randomUUID() : "default",
        name: "",
        enabled: true,
        cipher: cipher!,
        tokenHash: digest!,
        lastAttempt: 0,
      };
      l.accounts.push(a);
    }
    if (typeof input.name === "string") a.name = input.name.trim();
    if (typeof input.enabled === "boolean") a.enabled = input.enabled;
    if (digest && cipher) {
      assert(
        !l.keys.some((k) => (k.buckets[a!.id]?.pending ?? 0) > 0) ||
          a.tokenHash === digest,
        "reconcile pending Opus before replacing account",
        409,
      );
      const changed = a.tokenHash !== digest;
      a.cipher = cipher;
      a.tokenHash = digest;
      if (changed) {
        a.quota = undefined;
        a.opus = undefined;
        a.lastAttempt = 0;
        for (const k of l.keys) delete k.buckets[a.id];
      }
    }
    rebalance(l);
    this.db.saveLedger(l);
    return json(await this.publicAccount(a, l), id ? 200 : 201);
  }
}
