import "@vitest/runner";
declare module "@vitest/runner" {
  interface TaskMeta {
    loadMetrics?: Record<string, unknown>;
  }
}
