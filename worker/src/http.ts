import { assert, HttpError, object } from "./model";
export function json(
  value: unknown,
  status = 200,
  headers: HeadersInit = {},
): Response {
  return Response.json(value, {
    status,
    headers: { "Cache-Control": "no-store", ...headers },
  });
}
export function errorResponse(error: unknown): Response {
  const status = error instanceof HttpError ? error.status : 500;
  return new Response(
    error instanceof HttpError ? error.message : "internal service error",
    {
      status,
      headers: {
        "Cache-Control": "no-store",
        ...(status === 429 ? { "Retry-After": "15" } : {}),
      },
    },
  );
}
export async function readLimited(
  body: ReadableStream<Uint8Array> | null,
  limit: number,
): Promise<Uint8Array> {
  if (!body) return new Uint8Array();
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.length;
      assert(length <= limit, "request body too large", 413);
      chunks.push(value);
    }
  } catch (e) {
    await reader.cancel().catch(() => {});
    throw e;
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(length);
  let at = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, at);
    at += chunk.length;
  }
  return bytes;
}
export async function adminBody(
  request: Request,
): Promise<Record<string, unknown>> {
  try {
    return object(
      JSON.parse(
        new TextDecoder().decode(await readLimited(request.body, 8192)),
      ),
    );
  } catch (e) {
    if (e instanceof HttpError) throw e;
    throw new HttpError(400, "invalid JSON");
  }
}
export function fields(input: Record<string, unknown>, names: string[]): void {
  assert(
    Object.keys(input).every((f) => names.includes(f)),
    "unsupported field",
  );
}
export function bearer(request: Request): string {
  const match = /^Bearer ([^\s]+)$/i.exec(
    request.headers.get("Authorization") ?? "",
  );
  assert(match, "unauthorized", 401);
  return match[1];
}
