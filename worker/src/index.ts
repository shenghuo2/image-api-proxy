import { errorResponse } from "./http";
export { ProxyCoordinator } from "./proxy";
export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    try {
      const url = new URL(request.url),
        proxy = env.PROXY.getByName("global-v1");
      if (
        /^\/(admin|ai|image|user|quota|healthz|jobs)(\/|$)/.test(url.pathname)
      )
        return proxy.fetch(request);
      if (
        !["GET", "HEAD"].includes(request.method) ||
        url.pathname.includes("%")
      )
        return new Response("not found", { status: 404 });
      const path = await proxy.uiPath();
      if (url.pathname === path)
        return Response.redirect(url.origin + path + "/", 308);
      if (!url.pathname.startsWith(path + "/"))
        return new Response("not found", { status: 404 });
      const relative = url.pathname.slice(path.length),
        asset = new URL(
          relative === "/" ? "/index.html" : relative,
          url.origin,
        );
      const response = await env.ASSETS.fetch(
          new Request(asset, { method: request.method }),
        ),
        headers = new Headers(response.headers);
      headers.set("X-Content-Type-Options", "nosniff");
      headers.set("Referrer-Policy", "no-referrer");
      if (relative === "/") headers.set("Cache-Control", "no-store");
      return new Response(response.body, { status: response.status, headers });
    } catch (e) {
      return errorResponse(e);
    }
  },
} satisfies ExportedHandler<Env>;
