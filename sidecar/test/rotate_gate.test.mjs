import { describe, expect, test } from "bun:test";
import {
  LONG_PARK_MS,
  PROACTIVE_MIN_BACKENDS,
  PROACTIVE_MIN_LIVE,
  isLongPark,
  shouldProactiveRotate,
  shouldEscalateReuse,
} from "../lib/rotate_gate.mjs";

// The gate that decides whether a fleet is rotated before the client meets the
// all-429 wall. Boundary behaviour is the whole point: a healthy fleet must not
// be rotated (that spends accounts) and a nearly parked one must be.
describe("shouldProactiveRotate", () => {
  test("a healthy fleet is not rotated", () => {
    expect(shouldProactiveRotate(22, 0)).toBe(false);
    expect(shouldProactiveRotate(22, 15)).toBe(false); // 7 live > 6
  });

  test("the bound is inclusive: six live addresses rotate", () => {
    expect(shouldProactiveRotate(22, 16)).toBe(true); // 6 live
    expect(shouldProactiveRotate(22, 20)).toBe(true); // 2 live
    expect(shouldProactiveRotate(22, 22)).toBe(true); // none live
  });

  test("a pool too small to have a reserve is never rotated by this gate", () => {
    expect(shouldProactiveRotate(PROACTIVE_MIN_BACKENDS - 1, 9)).toBe(false);
    expect(shouldProactiveRotate(0, 0)).toBe(false);
  });

  test("minLive is a parameter, so the test can move the bound", () => {
    expect(shouldProactiveRotate(22, 1, 21)).toBe(true);
    expect(shouldProactiveRotate(22, 0, 21)).toBe(false);
  });
});

describe("isLongPark", () => {
  const now = 1_000_000;

  test("throttle and transport parks are transient", () => {
    expect(isLongPark(now, now)).toBe(false);
    expect(isLongPark(now + 5_000, now)).toBe(false); // throttle park
    expect(isLongPark(now + 60_000, now)).toBe(false); // transport park
  });

  test("a spent window is a long park", () => {
    expect(isLongPark(now + LONG_PARK_MS + 1, now)).toBe(true);
    expect(isLongPark(now + 11 * 3_600_000, now)).toBe(true); // hours-class 429
  });

  test("an expired park is not a park", () => {
    expect(isLongPark(now - 1, now)).toBe(false);
    expect(isLongPark(0, now)).toBe(false);
  });
});

describe("shouldEscalateReuse", () => {
  test("a single flapping slot is not a signal", () => {
    expect(shouldEscalateReuse(1, 22)).toBe(false);
    expect(shouldEscalateReuse(3, 22)).toBe(false);
  });

  test("a reuse burst on a quarter of the fleet escalates", () => {
    expect(shouldEscalateReuse(4, 22)).toBe(false); // 4 < 5.5
    expect(shouldEscalateReuse(6, 22)).toBe(true);
    expect(shouldEscalateReuse(13, 22)).toBe(true); // measured 18:44:17
  });

  test("an empty fleet or zero reuses never escalates", () => {
    expect(shouldEscalateReuse(0, 22)).toBe(false);
    expect(shouldEscalateReuse(4, 0)).toBe(false);
  });
});
