import { test, expect } from "@playwright/test";

// Deliberately trivial tests: the fixture's purpose is the JSON reporter's
// outcome/status shape (expected / unexpected+failed / flaky / skipped),
// not exercising a browser.

test("passes", async () => {
  expect(1).toBe(1);
});

test("always fails", async () => {
  expect(1).toBe(2);
});

// Attempt 1 fails, attempt 2 (retry) passes -> outcome "flaky".
test("flaky then passes", async () => {
  if (test.info().retry === 0) {
    expect(1).toBe(2);
  }
});

test.fixme("skipped", async () => {
  expect(1).toBe(1);
});
