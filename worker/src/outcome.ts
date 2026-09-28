/** Incremental MessagePack inspection. Binary image payloads are skipped, never decoded or retained. */
type Parser = Generator<number, unknown, Uint8Array>;
const empty = new Uint8Array();
const png = [137, 80, 78, 71, 13, 10, 26, 10];
const isPNG = (b: Uint8Array) => png.every((v, i) => b[i] === v);
class Frame {
  image = false;
  step = false;
  valid = true;
  done = false;
  private parser: Parser;
  private need = 0;
  private read = 0;
  private scratch = empty;
  constructor() {
    this.parser = this.value(0, "root");
    this.advance();
  }
  private advance(value = empty): void {
    const next = this.parser.next(value);
    this.done = !!next.done;
    if (!this.done) {
      this.need = Number(next.value);
      this.read = 0;
      this.scratch = this.need > 0 ? new Uint8Array(this.need) : empty;
    }
  }
  private *number(size: number, signed = false, float = false): Parser {
    const b = yield size;
    const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
    if (float) return size === 4 ? v.getFloat32(0) : v.getFloat64(0);
    if (size === 8)
      return Number(signed ? v.getBigInt64(0) : v.getBigUint64(0));
    if (size === 4) return signed ? v.getInt32(0) : v.getUint32(0);
    if (size === 2) return signed ? v.getInt16(0) : v.getUint16(0);
    return signed ? v.getInt8(0) : v.getUint8(0);
  }
  private *value(depth: number, field = ""): Parser {
    if (depth > 64) throw new Error("message nesting too deep");
    const tag = (yield 1)[0];
    let count = -1,
      map = false,
      length = -1,
      binary = false;
    if (tag <= 0x7f) return tag;
    if (tag >= 0xe0) return tag - 256;
    if (tag >= 0xa0 && tag <= 0xbf) length = tag & 31;
    else if (tag >= 0x80 && tag <= 0x8f) {
      count = tag & 15;
      map = true;
    } else if (tag >= 0x90 && tag <= 0x9f) count = tag & 15;
    else if (tag === 0xc0) return null;
    else if (tag === 0xc2 || tag === 0xc3) return tag === 0xc3;
    else if (tag >= 0xcc && tag <= 0xcf)
      return yield* this.number(2 ** (tag - 0xcc));
    else if (tag >= 0xd0 && tag <= 0xd3)
      return yield* this.number(2 ** (tag - 0xd0), true);
    else if (tag === 0xca || tag === 0xcb)
      return yield* this.number(tag === 0xca ? 4 : 8, false, true);
    else if (tag >= 0xd9 && tag <= 0xdb)
      length = Number(yield* this.number(2 ** (tag - 0xd9)));
    else if (tag >= 0xc4 && tag <= 0xc6) {
      length = Number(yield* this.number(2 ** (tag - 0xc4)));
      binary = true;
    } else if (tag === 0xdc || tag === 0xdd || tag === 0xde || tag === 0xdf) {
      count = Number(yield* this.number(tag % 2 === 0 ? 2 : 4));
      map = tag >= 0xde;
    } else if ((tag >= 0xd4 && tag <= 0xd8) || (tag >= 0xc7 && tag <= 0xc9)) {
      const n =
        tag >= 0xd4
          ? 2 ** (tag - 0xd4)
          : Number(yield* this.number(2 ** (tag - 0xc7)));
      yield -(n + 1);
      return undefined;
    } else throw new Error("invalid MessagePack tag");
    if (depth === 0 && !map) throw new Error("expected frame map");
    if (length >= 0) {
      if (length === 0) return binary ? undefined : "";
      if (binary) {
        if (field === "image" && depth === 1) {
          const n = Math.min(8, length);
          this.image = isPNG(yield n);
          if (length > n) yield -(length - n);
        } else yield -length;
        return undefined;
      }
      if (length > 256) {
        yield -length;
        return undefined;
      }
      return new TextDecoder().decode(yield length);
    }
    for (let i = 0; i < count; i++) {
      if (map) {
        const key = yield* this.value(depth + 1);
        const value = yield* this.value(
          depth + 1,
          depth === 0 ? String(key) : "",
        );
        if (
          depth === 0 &&
          key === "code" &&
          value != null &&
          String(value) !== "200"
        )
          this.valid = false;
        if (depth === 0 && key === "step_ix" && value != null) this.step = true;
      } else yield* this.value(depth + 1);
    }
    return undefined;
  }
  write(bytes: Uint8Array): void {
    let at = 0;
    while (at < bytes.length) {
      if (this.done) throw new Error("trailing frame bytes");
      const n = Math.min(Math.abs(this.need) - this.read, bytes.length - at);
      if (this.need > 0)
        this.scratch.set(bytes.subarray(at, at + n), this.read);
      at += n;
      this.read += n;
      if (this.read === Math.abs(this.need)) this.advance(this.scratch);
    }
  }
}
export class StreamOutcome {
  private header = new Uint8Array(4);
  private headerRead = 0;
  private left = 0;
  private frame?: Frame;
  private final = false;
  private invalid = false;
  write(bytes: Uint8Array): void {
    if (this.invalid) return;
    try {
      let at = 0;
      while (at < bytes.length) {
        if (!this.frame) {
          const n = Math.min(4 - this.headerRead, bytes.length - at);
          this.header.set(bytes.subarray(at, at + n), this.headerRead);
          this.headerRead += n;
          at += n;
          if (this.headerRead !== 4) continue;
          this.left = new DataView(this.header.buffer).getUint32(0);
          if (!this.left || this.left > 32 * 1024 * 1024)
            throw new Error("invalid frame length");
          this.frame = new Frame();
        }
        const n = Math.min(this.left, bytes.length - at);
        this.frame.write(bytes.subarray(at, at + n));
        at += n;
        this.left -= n;
        if (!this.left) {
          if (!this.frame.done || !this.frame.valid)
            throw new Error("incomplete/error frame");
          this.final ||= this.frame.image && !this.frame.step;
          this.frame = undefined;
          this.headerRead = 0;
        }
      }
    } catch {
      this.invalid = true;
    }
  }
  success(): boolean {
    return !this.invalid && this.final && !this.frame && this.headerRead === 0;
  }
}
