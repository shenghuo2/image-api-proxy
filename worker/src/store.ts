import {
  CACHE_LIMIT,
  emptyLedger,
  IDENTITY_TTL,
  MiB,
  type Job,
  type Ledger,
} from "./model";
/** All statements are measured, including deletes and index maintenance. No binary indexes. */
export class Store {
  reads = 0;
  writes = 0;
  alarmReads = 0;
  alarmWrites = 0;
  constructor(private state: DurableObjectState) {
    this.exec(
      "CREATE TABLE IF NOT EXISTS meta (id TEXT PRIMARY KEY, data TEXT NOT NULL)",
    );
    this
      .exec(`CREATE TABLE IF NOT EXISTS jobs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
      owner TEXT NOT NULL, idem TEXT, state TEXT NOT NULL, reserved INTEGER NOT NULL, expires INTEGER NOT NULL,
      finished INTEGER NOT NULL, data TEXT NOT NULL, UNIQUE(owner,idem))`);
    this.exec("CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state,seq)");
    this.exec("CREATE INDEX IF NOT EXISTS jobs_expiry ON jobs(expires)");
    this.exec("CREATE INDEX IF NOT EXISTS jobs_finished ON jobs(finished)");
    this.exec(
      "CREATE TABLE IF NOT EXISTS chunks (job TEXT NOT NULL, kind TEXT NOT NULL, part INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY(job,kind,part)) WITHOUT ROWID",
    );
    this.exec(
      "CREATE TABLE IF NOT EXISTS usage (quarter INTEGER PRIMARY KEY, images INTEGER NOT NULL, generations INTEGER NOT NULL)",
    );
    const version = this.exec<{ data: string }>(
      "SELECT data FROM meta WHERE id=?",
      "version",
    )[0];
    if (version && version.data !== "1")
      throw new Error("unsupported database version");
    if (!version)
      this.exec("INSERT INTO meta(id,data) VALUES(?,?)", "version", "1");
  }
  exec<
    T extends Record<string, SqlStorageValue> = Record<string, SqlStorageValue>,
  >(sql: string, ...args: SqlStorageValue[]): T[] {
    const cursor = this.state.storage.sql.exec<T>(sql, ...args);
    const values = cursor.toArray();
    this.reads += cursor.rowsRead;
    this.writes += cursor.rowsWritten;
    return values;
  }
  tx<T>(run: () => T): T {
    return this.state.storage.transactionSync(run);
  }
  ledger(): Ledger {
    const r = this.exec<{ data: string }>(
      "SELECT data FROM meta WHERE id=?",
      "ledger",
    )[0];
    return r ? JSON.parse(r.data) : emptyLedger();
  }
  saveLedger(l: Ledger): void {
    this.exec(
      "INSERT INTO meta(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data",
      "ledger",
      JSON.stringify(l),
    );
  }
  get(id: string): Job | undefined {
    const r = this.exec<{ data: string }>(
      "SELECT data FROM jobs WHERE id=?",
      id,
    )[0];
    return r ? JSON.parse(r.data) : undefined;
  }
  byIdem(owner: string, idem: string): Job | undefined {
    const r = this.exec<{ data: string }>(
      "SELECT data FROM jobs WHERE owner=? AND idem=?",
      owner,
      idem,
    )[0];
    return r ? JSON.parse(r.data) : undefined;
  }
  add(j: Job): void {
    this.exec(
      "INSERT INTO jobs(id,owner,idem,state,reserved,expires,finished,data) VALUES(?,?,?,?,?,?,?,?)",
      j.id,
      j.key,
      j.idem,
      j.state,
      j.reserved,
      j.expires,
      j.finished,
      JSON.stringify(j),
    );
  }
  save(j: Job): void {
    this.exec(
      "UPDATE jobs SET state=?,reserved=?,expires=?,finished=?,data=? WHERE id=?",
      j.state,
      j.reserved,
      j.expires,
      j.finished,
      JSON.stringify(j),
      j.id,
    );
  }
  waiting(): Job[] {
    return this.exec<{ data: string }>(
      "SELECT data FROM jobs WHERE state=? ORDER BY seq",
      "waiting",
    ).map((r) => JSON.parse(r.data));
  }
  running(): Job[] {
    return this.exec<{ data: string }>(
      "SELECT data FROM jobs WHERE state=? ORDER BY seq",
      "running",
    ).map((r) => JSON.parse(r.data));
  }
  reserved(): number {
    return Number(
      this.exec<{ n: number }>(
        "SELECT COALESCE(SUM(reserved),0) n FROM jobs",
      )[0].n,
    );
  }
  putChunk(id: string, kind: string, part: number, data: Uint8Array): void {
    this.exec(
      "INSERT INTO chunks(job,kind,part,data) VALUES(?,?,?,?)",
      id,
      kind,
      part,
      new Uint8Array(data).buffer,
    );
  }
  putBody(id: string, bytes: Uint8Array): void {
    for (let at = 0, part = 0; at < bytes.length; at += MiB, part++)
      this.putChunk(id, "body", part, bytes.subarray(at, at + MiB));
  }
  chunk(id: string, kind: string, part: number): Uint8Array | undefined {
    const row = this.exec<{ data: ArrayBuffer }>(
      "SELECT data FROM chunks WHERE job=? AND kind=? AND part=?",
      id,
      kind,
      part,
    )[0];
    return row ? new Uint8Array(row.data) : undefined;
  }
  body(j: Job): Uint8Array {
    const result = new Uint8Array(j.bodyBytes);
    for (let at = 0, part = 0; at < result.length; at += MiB, part++) {
      const data = this.chunk(j.id, "body", part);
      if (!data) throw new Error("missing body chunk");
      result.set(data, at);
    }
    return result;
  }
  deleteChunks(id: string, kind?: string): void {
    if (kind) this.exec("DELETE FROM chunks WHERE job=? AND kind=?", id, kind);
    else this.exec("DELETE FROM chunks WHERE job=?", id);
  }
  remove(id: string): void {
    this.deleteChunks(id);
    this.exec("DELETE FROM jobs WHERE id=?", id);
  }
  recover(now: number): void {
    this.tx(() => {
      for (const j of [
        ...this.running(),
        ...this.waiting().filter((j) => !j.durable),
      ]) {
        if (!j.durable) {
          this.remove(j.id);
          continue;
        }
        j.state = "interrupted";
        j.error = "execution interrupted; not retried";
        j.finished = now;
        j.reserved = 0;
        this.deleteChunks(j.id);
        this.save(j);
      }
    });
  }
  cleanup(now: number): void {
    this.tx(() => {
      const expired = this.exec<{ data: string }>(
        "SELECT data FROM jobs WHERE expires>0 AND expires<=? LIMIT 128",
        now,
      );
      for (const row of expired) {
        const j: Job = JSON.parse(row.data);
        this.deleteChunks(j.id);
        j.reserved = 0;
        j.resultBytes = 0;
        // Zero the indexed timestamp after cleanup; expiry remains in JSON for 410 responses.
        this.save(j);
        this.exec("UPDATE jobs SET expires=0 WHERE id=?", j.id);
      }
      for (const row of this.exec<{ id: string }>(
        "SELECT id FROM jobs WHERE finished>0 AND finished<=? LIMIT 128",
        now - IDENTITY_TTL,
      ))
        this.remove(row.id);
      this.exec(
        "DELETE FROM usage WHERE quarter<?",
        Math.floor((now - 8 * 86400_000) / 1000),
      );
    });
  }
  nextExpiry(): number | undefined {
    const r = this.exec<{ expires: number }>(
      "SELECT expires FROM jobs WHERE expires>0 ORDER BY expires LIMIT 1",
    )[0];
    const f = this.exec<{ finished: number }>(
      "SELECT finished FROM jobs WHERE finished>0 ORDER BY finished LIMIT 1",
    )[0];
    const times = [
      r?.expires,
      f ? f.finished + IDENTITY_TTL : undefined,
    ].filter((n): n is number => n !== undefined);
    return times.length ? Math.min(...times) : undefined;
  }
  record(samples: number, now: number): void {
    this.exec(
      "INSERT INTO usage(quarter,images,generations) VALUES(?,?,1) ON CONFLICT(quarter) DO UPDATE SET images=images+excluded.images,generations=generations+1",
      Math.floor(now / 900_000) * 900,
      samples,
    );
  }
  metrics() {
    return {
      rows_read: this.reads + this.alarmReads,
      sql_rows_read: this.reads,
      rows_written: this.writes + this.alarmWrites,
      sql_rows_written: this.writes,
      alarm_reads: this.alarmReads,
      alarm_writes: this.alarmWrites,
      database_bytes: this.state.storage.sql.databaseSize,
      reserved_bytes: this.reserved(),
      capacity_bytes: CACHE_LIMIT,
    };
  }
}
