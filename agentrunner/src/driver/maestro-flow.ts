// driver/maestro-flow.ts — decision → Maestro flow YAML, shared by the
// Android and iOS drivers. Rendered by hand (no YAML dependency): the
// command surface here is intentionally tiny (tapOn/inputText/pressKey/
// launchApp), so a template string is cheaper than a serialization lib.
//
// Mapper choices (documented per T3):
//   - navigate never goes through maestro — lifecycle commands are
//     single-shot: Android uses `adb shell am start -a VIEW -d <url>`
//     (deep link or http), iOS uses `xcrun simctl openurl`.
//   - act: tapOn/inputText/pressKey map 1:1 onto Maestro's core synthetic
//     input commands. Selector → `tapOn: id:` (resource-id on Android,
//     accessibility id on iOS via maestro); label → `tapOn` text matcher.
//   - launchApp is prepended when the caller configured an app id
//     (E2AI_ANDROID_APP_ID / E2AI_IOS_APP_ID) so the flow starts from a
//     known app state.
// Unmappable decisions throw — the step fails as an app-level error, never
// misclassified as infra.

import type { Decision } from "../loop.js";

/** The flow body steps for one act decision. */
export function maestroFlowSteps(decision: Decision): string[] {
  if (decision.kind !== "act") {
    throw new Error(
      `maestro flow mapper handles act decisions only, got kind ${JSON.stringify(decision.kind)}`,
    );
  }
  const selector = typeof decision.selector === "string" ? decision.selector : undefined;
  const text = typeof decision.text === "string" ? decision.text : undefined;
  const label = typeof decision.label === "string" ? decision.label : undefined;
  const key = typeof decision.key === "string" ? decision.key : undefined;

  if (selector && text) {
    return [`- tapOn:\n    id: ${selector}`, `- inputText: ${JSON.stringify(text)}`];
  }
  if (selector) return [`- tapOn:\n    id: ${selector}`];
  if (label) return [`- tapOn: ${JSON.stringify(label)}`];
  if (key) return [`- pressKey: ${key}`];
  throw new Error(`cannot map decision to a maestro command: ${JSON.stringify(decision)}`);
}

/** Full YAML body for a one-action flow file fed to `maestro test`. */
export function maestroFlowYaml(decision: Decision, opts?: { appId?: string }): string {
  const header = opts?.appId
    ? [
        // maestro >= 2.10 rejects a bare command list ("Config Section
        // Required"): every flow needs a config section + `---` separator.
        // A `name`-only section does not satisfy it — appId is the minimal
        // accepted field (probed against maestro 2.10.0, plan 04-01 T5).
        `appId: ${opts.appId}\n---`,
        `- launchApp:\n    appId: ${opts.appId}\n    clearState: false`,
      ]
    : [];
  return [...header, ...maestroFlowSteps(decision)].join("\n");
}
