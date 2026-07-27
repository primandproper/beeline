// Bundled read-load generator for `make demo`. The head pool scales on CPU, so
// without something driving traffic its HPA sits at minReplicas forever and half
// the demo has nothing to show. This is that something.
//
// Turn it up live:
//   kubectl --context k3d-beeline-local -n beeline-local \
//     set env deploy/beeline-loadgen RPS=250
//
// Two details are load-bearing, and changing either quietly breaks the demo:
//
//  1. `constant-arrival-rate`, not a request loop. An open model fires at a fixed
//     rate regardless of response time. A closed loop's throughput is
//     concurrency/latency, so its "RPS knob" would silently collapse into a
//     latency knob exactly when the heads got busy — the opposite of what a load
//     generator is for.
//
//  2. `fill: false` on /table. With fill enabled a miss demand-computes through
//     the area's provider, which under the demo's `latent-haversine` sleeps
//     25-120ms — and a sleeping goroutine moves no CPU metric. The HPA would sit
//     flat while p95 exploded. With fill off, a miss returns null and the request
//     is pure cache read + H3 indexing + JSON encoding of ~1,150 floats: real,
//     sustained CPU, which is what a CPU autoscaler needs to see.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE || 'http://beeline-head:8080';
const RPS = Number(__ENV.RPS || 25);
const GRID = Number(__ENV.TABLE_GRID || 24); // GRID x GRID cells per /table call
const RATIO = Number(__ENV.TABLE_RATIO || 0.25); // share of iterations that are /table

// Bounding box of the Downtown Austin polygon in scripts/seed_demo_areas.sh:
// one res-9 layer at full mesh, the densest and most reliably-cached area.
const LAT0 = 30.2575;
const LAT1 = 30.2838;
const LNG0 = -97.7563;
const LNG1 = -97.7305;

export const options = {
  discardResponseBodies: true,
  // Every /estimate URL carries unique coordinates, so leaving `url` in the
  // system tags gives k6 one time series PER REQUEST. Left alone it crosses
  // 100k series in about a minute, and the metrics engine then eats both the
  // memory and the throughput — the generator starves itself long before the
  // heads notice. Drop `url`; the per-request `name` tag below does the
  // grouping instead.
  systemTags: ['status', 'method', 'name', 'scenario', 'error'],
  // No thresholds on purpose: a demo must never exit non-zero because p95 drifted.
  scenarios: {
    reads: {
      executor: 'constant-arrival-rate',
      rate: RPS,
      timeUnit: '1s',
      // Deliberately NOT "run forever": k6 accumulates some state for the length
      // of a run, so a bounded one plus the container restart that follows it
      // resets the arena. The couple of seconds of gap are well inside the head
      // HPA's stabilization window. (The OOMKills this was first reached for
      // turned out to be maxVUs — see below — but a bound is still cheap
      // insurance against a slow leak in a pod meant to run for days.)
      duration: '30m',
      // An open model needs VUs ~= rate x latency, and a cached read is ~12ms, so
      // this ceiling is enormous headroom at steady state.
      //
      // Do NOT raise it to silence the "Insufficient VUs" warning. Under a
      // deliberate overload that warning is the system under test talking: the
      // heads slowed down, so each iteration holds its VU longer, so the pool
      // runs dry. Raising the ceiling just buys more VUs to queue in — measured
      // at 6x, it tripled the generator's memory and produced the identical
      // head CPU and the identical scale-up. Every VU is a separate JS runtime
      // costing megabytes, so the only thing an over-generous ceiling reliably
      // achieves is OOMKilling the pod under the load it was raised to survive.
      preAllocatedVUs: Math.max(20, Math.ceil(RPS / 4)),
      maxVUs: Math.max(100, RPS * 2),
    },
  },
};

function pt() {
  const lat = LAT0 + Math.random() * (LAT1 - LAT0);
  const lng = LNG0 + Math.random() * (LNG1 - LNG0);
  return `${lat.toFixed(5)},${lng.toFixed(5)}`;
}

export default function () {
  if (Math.random() < RATIO) {
    // A sparse batch read. 24x24 = 576 cells, well inside the maxTableCells guard.
    const sources = [];
    const destinations = [];
    for (let i = 0; i < GRID; i++) {
      sources.push(pt());
      destinations.push(pt());
    }
    const res = http.post(
      `${BASE}/table`,
      JSON.stringify({ sources, destinations, fill: false }),
      { headers: { 'content-type': 'application/json' }, tags: { name: 'table' } },
    );
    check(res, { 'table ok': (r) => r.status === 200 });
  } else {
    const res = http.get(`${BASE}/estimate?origin=${pt()}&dest=${pt()}`, {
      tags: { name: 'estimate' },
    });
    check(res, { 'estimate ok': (r) => r.status === 200 });
  }
}
