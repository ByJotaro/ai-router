// Rotation gate shared by the sidecar and its tests.
//
// Why this is a separate module: both gates used to require liveStreams === 0,
// and on a busy agent session that counter never reaches zero (it sat at
// 298-299 for two hours), so a fully parked fleet stayed parked: measured
// 2026-10-30 17:26-17:36, ten minutes of all_egress_429 -> client 503 while a
// rotate would have been free.
//
// The proactive gate fires while a few addresses still serve, so the client
// never meets the wall. It counts only LONG parks: a 5 s throttle or a 60 s
// transport park after an uplink blip parks the whole fleet for a minute, and
// rotating on that hiccup would spend a whole fleet of accounts for nothing.

export const PROACTIVE_MIN_LIVE = 6;
// A pool smaller than this is not a fleet: rotating it cannot add reserve.
export const PROACTIVE_MIN_BACKENDS = 10;
// Parks shorter than this are transient (throttle 5 s, transport 60 s).
export const LONG_PARK_MS = 5 * 60_000;

export function shouldProactiveRotate(backendsLen, longParked, minLive = PROACTIVE_MIN_LIVE) {
  if (!(backendsLen >= PROACTIVE_MIN_BACKENDS)) return false;
  return backendsLen - longParked <= minLive;
}

export function isLongPark(until, now, threshold = LONG_PARK_MS) {
  return (until || 0) - now > threshold;
}

// A redraw that lands on addresses the book already knows is not a redraw: the
// egress is fixed per WARP device, so a retunnel can only hand back exits this
// path already burned. Measured 2026-10-30 18:44-18:52: 13 of 22 slots came
// back reused, the landed sets carried 8-10 dead-on-arrival slots, and nothing
// escalated because the mint gate wanted spent >= 0.60 (it was 0.36-0.45).
export const REUSE_ESCALATE_N = 4;
export const REUSE_ESCALATE_SHARE = 0.25;

export function shouldEscalateReuse(reuses, fleet, n = REUSE_ESCALATE_N, share = REUSE_ESCALATE_SHARE) {
  if (!(reuses > 0) || !(fleet > 0)) return false;
  return reuses >= n && reuses >= fleet * share;
}
