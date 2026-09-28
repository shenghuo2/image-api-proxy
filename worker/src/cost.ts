import { assert, object, type Cost } from "./model";
const models = new Set([
  "nai-diffusion-3",
  "nai-diffusion-3-inpainting",
  "nai-diffusion-4-full",
  "nai-diffusion-4-full-inpainting",
  "nai-diffusion-4-curated-preview",
  "nai-diffusion-4-curated-preview-inpainting",
  "nai-diffusion-4-5-full",
  "nai-diffusion-4-5-full-inpainting",
  "nai-diffusion-4-5-curated",
  "nai-diffusion-4-5-curated-inpainting",
  "nai-diffusion-5-full",
  "nai-diffusion-5-full-inpainting",
  "nai-diffusion-5-curated",
]);
export const routes = new Set([
  "/ai/generate-image",
  "/ai/generate-image-stream",
  "/ai/upscale",
  "/ai/encode-vibe",
  "/ai/augment-image",
]);
export const canonical = (p: string) =>
  p.startsWith("/image/") ? p.slice(6) : p;
const dimension = (n: unknown) =>
  typeof n === "number" && Number.isInteger(n) && n >= 64 && n <= 2048;
export async function estimate(
  route: string,
  bytes: Uint8Array,
  contentType: string,
): Promise<Cost> {
  let raw: string;
  if (contentType.toLowerCase().startsWith("multipart/")) {
    // Only parse a bounded request, preserving original multipart bytes for forwarding.
    const form = await new Response(new Uint8Array(bytes), {
      headers: { "Content-Type": contentType },
    }).formData();
    const parts = form.getAll("request");
    assert(parts.length === 1, "expected one request part");
    raw = typeof parts[0] === "string" ? parts[0] : await parts[0].text();
  } else raw = new TextDecoder().decode(bytes);
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error("invalid JSON");
  }
  const p = object(parsed);
  if (route === "/ai/encode-vibe")
    return { full: 2, extras: 0, opus: false, v5: false, samples: 0 };
  if (route === "/ai/upscale")
    return { full: 200, extras: 0, opus: false, v5: false, samples: 0 };
  if (route === "/ai/augment-image") {
    assert(
      typeof p.image === "string" &&
        p.image.length &&
        dimension(p.width) &&
        dimension(p.height),
      "invalid augmentation",
    );
    assert(
      [
        "emotion",
        "bg-removal",
        "colorize",
        "declutter",
        "lineart",
        "sketch",
      ].includes(String(p.req_type)),
      "invalid augmentation type",
    );
    const pixels = Math.max(65536, Number(p.width) * Number(p.height));
    let base = Math.max(
      Math.ceil(
        2.951823174884865e-6 * pixels + 5.753298233447344e-7 * pixels * 28,
      ),
      2,
    );
    if (p.req_type === "bg-removal") base = base * 3 + 5;
    return {
      full: Math.ceil(base * 1.2) + 5,
      extras: 0,
      opus: false,
      v5: false,
      samples: 0,
    };
  }
  assert(models.has(String(p.model)), "unsupported generation model");
  const q = object(p.parameters);
  assert(dimension(q.width) && dimension(q.height), "unsupported dimensions");
  assert(
    typeof q.steps === "number" &&
      Number.isInteger(q.steps) &&
      q.steps >= 1 &&
      q.steps <= 50,
    "unsupported steps",
  );
  assert(
    typeof q.n_samples === "number" &&
      Number.isInteger(q.n_samples) &&
      q.n_samples >= 1 &&
      q.n_samples <= 4,
    "unsupported samples",
  );
  const count = (f: string) => {
    if (q[f] === undefined || q[f] === null) return 0;
    assert(Array.isArray(q[f]), `invalid ${f}`);
    return q[f].length;
  };
  const vibes =
    count("reference_image_multiple") +
    count("reference_image_multiple_cached");
  const refs =
    count("director_reference_images") +
    count("director_reference_images_cached");
  assert(vibes <= 16 && refs <= 10, "too many references");
  const pixels = Number(q.width) * Number(q.height);
  const v5 = String(p.model).startsWith("nai-diffusion-5");
  let base = Math.ceil(
    2.951823174884865e-6 * pixels + 5.753298233447344e-7 * pixels * q.steps,
  );
  if (v5) base = Math.ceil(base * 1.5);
  const extra = Math.max(0, vibes - 4) * 2 + refs * 5;
  return {
    full: (Math.ceil((base + extra) * 1.2) + 5) * q.n_samples,
    extras: extra > 0 ? Math.ceil(extra * 1.2) + 2 : 0,
    opus: q.n_samples === 1 && q.steps <= 28 && pixels <= 1048576,
    v5,
    samples: q.n_samples,
  };
}
