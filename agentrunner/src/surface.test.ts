// surface.test.ts — the SurfaceDriver seam: locked surface vocabulary
// (parse/zero-default/reject), factory selection, and loop-side config-error
// typing for unknown E2AI_SURFACE values.
import test from "node:test";
import assert from "node:assert/strict";

import { AndroidDriver } from "./driver/maestro-android.js";
import { IOSDriver } from "./driver/simctl-ios.js";
import { WebDriver } from "./driver/web.js";
import { InfraFlakeError, parseSurface, selectDriver, SurfaceConfigError } from "./surface.js";

test("parseSurface: lowercase zero-default, trims/case-folds the locked vocabulary", () => {
  assert.equal(parseSurface(undefined), "web");
  assert.equal(parseSurface(null), "web");
  assert.equal(parseSurface(""), "web");
  assert.equal(parseSurface("  "), "web");
  assert.equal(parseSurface("web"), "web");
  assert.equal(parseSurface("ANDROID"), "android");
  assert.equal(parseSurface(" iOS "), "ios");
});

test("parseSurface: unknown value is a config bug — SurfaceConfigError, never silent web", () => {
  assert.throws(() => parseSurface("desktop"), SurfaceConfigError);
  assert.throws(() => parseSurface("Web_Area"), SurfaceConfigError);
});

test("selectDriver maps the locked vocabulary to the concrete driver class", () => {
  assert.ok(selectDriver("web", {}) instanceof WebDriver);
  assert.ok(selectDriver("android", {}) instanceof AndroidDriver);
  assert.ok(selectDriver("ios", {}) instanceof IOSDriver);
});

test("InfraFlakeError carries the locked failure class + failing tool", () => {
  const err = new InfraFlakeError("infra_flake:device_offline", "adb", "no devices");
  assert.equal(err.failureClass, "infra_flake:device_offline");
  assert.equal(err.tool, "adb");
  assert.match(err.message, /infra_flake \(infra_flake:device_offline\) on adb: no devices/);
  // non-string detail is JSON-stringified, never "[object Object]"
  assert.match(
    new InfraFlakeError("infra_flake:probe_timeout", "xcrun", { code: 7 }).message,
    /"code":7/,
  );
});
