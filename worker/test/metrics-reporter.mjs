import { mkdirSync, writeFileSync, readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

// Linux process high-water RSS includes SQLite, workerd and the test harness.
// It is deliberately NOT presented as the production isolate's 128 MB usage.
function workerdPeakRSS() {
  if (process.platform !== "linux") return null;
  const processes = new Map();
  for (const pid of readdirSync("/proc").filter((p) => /^\d+$/.test(p))) {
    try {
      const status = readFileSync(`/proc/${pid}/status`, "utf8");
      processes.set(Number(pid), {
        parent: Number(/^PPid:\s*(\d+)/m.exec(status)?.[1]),
        name: /^Name:\s*(\S+)/m.exec(status)?.[1],
        peak: Number(/^VmHWM:\s*(\d+)/m.exec(status)?.[1] ?? 0) * 1024,
      });
    } catch {
      /* The process may exit during enumeration. */
    }
  }
  let peak = 0;
  for (const [pid, data] of processes) {
    if (data.name !== "workerd") continue;
    let ancestor = pid;
    while (ancestor && ancestor !== process.pid)
      ancestor = processes.get(ancestor)?.parent ?? 0;
    if (ancestor === process.pid) peak = Math.max(peak, data.peak);
  }
  return peak || null;
}

export default class MetricsReporter {
  onTestCaseResult(test) {
    const metrics = test.meta().loadMetrics;
    if (!metrics) return;
    const report = {
      ...metrics,
      peak_workerd_process_rss_bytes: workerdPeakRSS(),
      memory_note:
        "Host process RSS includes SQLite, runtime and test harness. Isolate heap measurement unavailable when peak_heap_bytes is zero; real Free-plan memory/CPU still require deployment validation.",
    };
    const directory = resolve("artifacts");
    mkdirSync(directory, { recursive: true });
    writeFileSync(
      resolve(directory, "load.json"),
      JSON.stringify(report, null, 2) + "\n",
    );
    console.log("LOAD_METRICS " + JSON.stringify(report));
  }
}
