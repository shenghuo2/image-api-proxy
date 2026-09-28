import { env } from "cloudflare:workers";
import { reset } from "cloudflare:test";
import { afterEach, expect, it } from "vitest";
import worker from "../src/index";
afterEach(async () => {
  await reset();
});
it("serves the console only at the configured path and hides the old path immediately", async () => {
  const response = await worker.fetch(
    new Request("https://proxy.test/console/"),
    env,
  );
  expect(response.status).toBe(200);
  expect(response.headers.get("Content-Type")).toContain("text/html");
  const html = await response.text();
  expect(html).toContain('<div id="root">');
  const script = /src="([^"]+\.js)"/.exec(html)![1];
  expect(
    (
      await worker.fetch(
        new Request(new URL(script, "https://proxy.test/console/")),
        env,
      )
    ).status,
  ).toBe(200);
  expect(
    (await worker.fetch(new Request("https://proxy.test/"), env)).status,
  ).toBe(404);
  const update = await worker.fetch(
    new Request("https://proxy.test/admin/settings", {
      method: "PUT",
      headers: {
        Authorization: "Bearer test-admin-key-not-a-real-secret-1234567890",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ admin_ui_path: "/private/console" }),
    }),
    env,
  );
  expect(update.status).toBe(200);
  expect(
    (await worker.fetch(new Request("https://proxy.test/console/"), env))
      .status,
  ).toBe(404);
  expect(
    (
      await worker.fetch(
        new Request("https://proxy.test/private/console/"),
        env,
      )
    ).status,
  ).toBe(200);
  const redirect = await worker.fetch(
    new Request("https://proxy.test/private/console"),
    env,
  );
  expect(redirect.status).toBe(308);
  expect(redirect.headers.get("Location")).toBe(
    "https://proxy.test/private/console/",
  );
});
