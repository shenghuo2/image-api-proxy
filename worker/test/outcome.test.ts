import { describe, it, expect } from "vitest";
import { StreamOutcome } from "../src/outcome";
// Hand-encoded NovelAI MessagePack maps, with the same length prefix as the Go fixtures.
export function frame(final = true, bytes = 8, code = 200): Uint8Array {
  const head = new Uint8Array([
    final ? 0x82 : 0x83,
    0xa4,
    99,
    111,
    100,
    101,
    0xcd,
    code >> 8,
    code & 255,
    ...(!final ? [0xa7, 115, 116, 101, 112, 95, 105, 120, 1] : []),
    0xa5,
    105,
    109,
    97,
    103,
    101,
    0xc6,
    bytes >>> 24,
    (bytes >>> 16) & 255,
    (bytes >>> 8) & 255,
    bytes & 255,
  ]);
  const b = new Uint8Array(4 + head.length + bytes);
  new DataView(b.buffer).setUint32(0, head.length + bytes);
  b.set(head, 4);
  b.set([137, 80, 78, 71, 13, 10, 26, 10].slice(0, bytes), 4 + head.length);
  return b;
}
describe("bounded frame inspection", () => {
  it("recognizes final PNG frames across every possible boundary", () => {
    const bytes = frame();
    for (let split = 0; split < bytes.length; split++) {
      const o = new StreamOutcome();
      o.write(bytes.subarray(0, split));
      o.write(bytes.subarray(split));
      expect(o.success()).toBe(true);
    }
  });
  it("ignores progress frames and rejects error/truncated/malformed frames", () => {
    for (const data of [
      frame(false),
      frame(true, 8, 500),
      frame().subarray(0, 10),
      new Uint8Array([255, 255, 255, 255]),
    ]) {
      const o = new StreamOutcome();
      o.write(data);
      expect(o.success()).toBe(false);
    }
  });
  it("skips a large binary payload in bounded chunks and accepts a later final frame", () => {
    const bytes = frame(false, 8 * 1024 * 1024),
      o = new StreamOutcome();
    for (let at = 0; at < bytes.length; at += 65536)
      o.write(bytes.subarray(at, at + 65536));
    expect(o.success()).toBe(false);
    o.write(frame());
    expect(o.success()).toBe(true);
  });
  it("rejects trailing bytes after a complete frame", () => {
    const o = new StreamOutcome();
    o.write(frame());
    o.write(new Uint8Array([1]));
    expect(o.success()).toBe(false);
  });
});
