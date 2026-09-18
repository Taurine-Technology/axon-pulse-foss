import assert from "node:assert/strict";
import test from "node:test";
import {setupUpdates} from "../frontend/updates.mjs";

function element() {
  const classes = new Set();
  return {
    disabled: false, textContent: "", value: "main", attributes: {}, listeners: {},
    classList: {add: (name) => classes.add(name), remove: (name) => classes.delete(name), contains: (name) => classes.has(name)},
    setAttribute(name, value) { this.attributes[name] = value; },
    addEventListener(name, handler) { this.listeners[name] = handler; },
    trigger(name) { return this.listeners[name](); },
  };
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return {promise, resolve, reject};
}

function harness(call) {
  const button = element(), message = element(), stream = element(), streamSetting = element(), managedHelp = element();
  const toasts = [], channels = [];
  const controls = setupUpdates({button, message, stream, streamSetting, managedHelp, call,
    t: (key, values = {}) => [key, ...Object.values(values)].join(" "),
    toast: (text) => toasts.push(text), onChannelChange: (channel) => channels.push(channel),
  });
  controls.renderStatus({state: "active", update_channel: "alpha"});
  return {button, message, stream, streamSetting, managedHelp, toasts, channels, ...controls};
}

test("checking stays visible and locked through live status updates and duplicate events", async () => {
  const gate = deferred(), calls = [];
  const h = harness((...args) => { calls.push(args); return gate.promise; });
  const pending = h.button.trigger("click");
  assert.equal(h.message.textContent, "update_check_progress");
  assert.equal(h.button.textContent, "checking");
  assert.equal(h.button.attributes["aria-busy"], "true");
  h.renderStatus({state: "active", update_channel: "alpha"});
  assert.equal(h.button.disabled, true);
  assert.equal(h.stream.disabled, true);
  assert.equal(h.button.textContent, "checking");
  await h.button.trigger("click");
  await h.stream.trigger("change");
  assert.deepEqual(calls, [["CheckUpdate"]]);
  gate.resolve({available_version: "0.0.8"});
  await pending;
  assert.equal(h.button.textContent, "install_update");
  assert.equal(h.message.textContent, "update_available 0.0.8");
  assert.equal(h.button.disabled, false);
  assert.equal(h.stream.disabled, false);
  assert.equal(h.button.attributes["aria-busy"], "false");
});

for (const restarting of [true, false]) {
  test(`download remains busy until ${restarting ? "restart" : "staging"} is confirmed`, async () => {
    const gate = deferred(), health = deferred(), calls = [];
    const h = harness((name) => {
      calls.push(name);
      if (name === "Status") return health.promise;
      return name === "CheckUpdate" ? Promise.resolve({available_version: "0.0.8"}) : gate.promise;
    });
    await h.button.trigger("click");
    const pending = h.button.trigger("click");
    assert.equal(h.button.textContent, "update_downloading");
    assert.equal(h.message.textContent, "update_download_progress");
    assert.equal(h.button.classList.contains("hidden"), false);
    h.renderStatus({state: "active", update_channel: "alpha"});
    assert.equal(h.button.disabled, true);
    assert.equal(h.stream.disabled, true);
    await h.button.trigger("click");
    assert.deepEqual(calls, ["CheckUpdate", "StageUpdate"]);
    gate.resolve({available_version: "0.0.8", restarting});
    if (restarting) {
      await Promise.resolve();
      assert.equal(h.message.textContent, "update_restarting 0.0.8");
      assert.equal(h.button.attributes["aria-busy"], "true");
      assert.equal(h.stream.disabled, true);
      health.resolve({version: "0.0.8", state: "active"});
    }
    await pending;
    assert.equal(h.message.textContent, `${restarting ? "update_installed" : "update_staged"} 0.0.8`);
    assert.equal(h.button.classList.contains("hidden"), !restarting);
    assert.equal(h.button.attributes["aria-busy"], "false");
    assert.deepEqual(h.toasts, ["update_verified"]);
    h.stream.value = "beta";
    const rechecking = h.stream.trigger("change");
    assert.equal(h.button.classList.contains("hidden"), false);
    assert.equal(h.button.textContent, "checking");
    await rechecking;
  });
}

test("download failure remains inline and allows retry without another check", async () => {
  const gate = deferred(), calls = [];
  const h = harness((name) => {
    calls.push(name);
    return name === "CheckUpdate" ? Promise.resolve({available_version: "0.0.8"}) : gate.promise;
  });
  await h.button.trigger("click");
  const pending = h.button.trigger("click");
  gate.reject(new Error("Download timed out"));
  await pending;
  assert.equal(h.message.textContent, "update_failed Download timed out");
  assert.equal(h.button.textContent, "install_update");
  assert.equal(h.button.disabled, false);
  assert.equal(h.button.classList.contains("hidden"), false);
  await h.button.trigger("click");
  assert.deepEqual(calls, ["CheckUpdate", "StageUpdate", "StageUpdate"]);
});

test("checking failure clears busy state and respects administrator and service locks", async () => {
  const gate = deferred();
  const h = harness(() => gate.promise);
  const pending = h.button.trigger("click");
  h.renderStatus({state: "service_unavailable", update_channel: "alpha", update_channel_locked: true});
  gate.reject(new Error("Service unavailable"));
  await pending;
  assert.equal(h.message.textContent, "update_failed Service unavailable");
  assert.equal(h.button.attributes["aria-busy"], "false");
  assert.equal(h.button.disabled, true);
  assert.equal(h.stream.disabled, true);
  h.renderStatus({state: "active", update_channel: "alpha", update_channel_locked: true});
  assert.equal(h.button.disabled, false);
  assert.equal(h.stream.disabled, true);
  assert.equal(h.button.textContent, "check_update");
});

test("changing stream locks checking and downloads until its own check completes", async () => {
  const gate = deferred(), calls = [];
  const h = harness((...args) => { calls.push(args); return gate.promise; });
  h.stream.value = "beta";
  const pending = h.stream.trigger("change");
  h.renderStatus({state: "active", update_channel: "alpha"});
  assert.equal(h.stream.value, "beta");
  assert.equal(h.button.disabled, true);
  assert.equal(h.message.textContent, "update_check_progress");
  await h.button.trigger("click");
  gate.resolve({channel: "beta", current_version: "0.0.8"});
  await pending;
  assert.deepEqual(calls, [["SetUpdateChannel", "beta"]]);
  assert.deepEqual(h.channels, ["beta"]);
  assert.equal(h.stream.value, "beta");
  assert.equal(h.message.textContent, "up_to_date 0.0.8");
  assert.equal(h.button.textContent, "check_update");
});

test("failed stream change restores the previous stream and leaves the error visible", async () => {
  const h = harness(() => Promise.reject(new Error("Connection lost")));
  h.stream.value = "beta";
  await h.stream.trigger("change");
  assert.equal(h.stream.value, "alpha");
  assert.equal(h.stream.disabled, false);
  assert.equal(h.message.textContent, "update_failed Connection lost");
  assert.deepEqual(h.channels, []);
});

test("a returned check error stays visible after switching streams", async () => {
  const h = harness(() => Promise.resolve({channel: "beta", check_error: "Index unavailable"}));
  h.stream.value = "beta";
  await h.stream.trigger("change");
  assert.equal(h.message.textContent, "update_check_failed stream_beta Index unavailable");
  assert.equal(h.stream.value, "beta");
  assert.equal(h.button.disabled, false);
});

test("APT status hides self-update controls and keeps package instructions through live events", async () => {
  const calls = [];
  const h = harness((...args) => { calls.push(args); });
  h.renderStatus({state: "active", update_method: "apt", version: "1.0.0"});
  assert.equal(h.button.classList.contains("hidden"), true);
  assert.equal(h.streamSetting.classList.contains("hidden"), true);
  assert.equal(h.managedHelp.classList.contains("hidden"), false);
  assert.equal(h.button.disabled, true);
  assert.equal(h.stream.disabled, true);
  assert.equal(h.message.textContent, "update_apt_managed 1.0.0");
  await h.button.trigger("click");
  await h.stream.trigger("change");
  h.renderStatus({state: "service_unavailable", update_method: "apt", version: "1.0.0"});
  assert.equal(h.managedHelp.classList.contains("hidden"), false);
  assert.deepEqual(calls, []);
});

test("a managed service that restarted into a newer package offers Restart Pulse", async () => {
  const calls = [];
  const h = harness(async (name) => { calls.push(name); return {}; });
  h.setDesktopVersion("1.0.0");
  h.renderStatus({state: "active", update_method: "apt", version: "1.0.0"});
  assert.equal(h.button.classList.contains("hidden"), true);
  h.renderStatus({state: "active", update_method: "apt", version: "1.0.1"});
  assert.equal(h.button.classList.contains("hidden"), false);
  assert.equal(h.button.disabled, false);
  assert.equal(h.button.textContent, "restart_pulse");
  assert.equal(h.message.textContent, "update_restart_needed 1.0.1");
  // Package instructions stay visible and the stream stays locked.
  assert.equal(h.managedHelp.classList.contains("hidden"), false);
  assert.equal(h.stream.disabled, true);
  await h.button.trigger("click");
  assert.deepEqual(calls, ["RelaunchDesktop"]);
});

test("a managed check result never claims the package is up to date", async () => {
  const h = harness(() => Promise.resolve({update_method: "apt", current_version: "1.0.0"}));
  await h.button.trigger("click");
  assert.equal(h.message.textContent, "update_apt_managed 1.0.0");
  assert.equal(h.button.classList.contains("hidden"), true);
  assert.equal(h.managedHelp.classList.contains("hidden"), false);
});

test("shows live download progress on the button while an update stages", async () => {
  const staging = deferred();
  const h = harness(async (name) => {
    if (name === "CheckUpdate") return {available_version: "0.0.7-alpha.5", channel: "alpha"};
    if (name === "StageUpdate") return staging.promise;
    throw new Error(`unexpected ${name}`);
  });
  await h.button.trigger("click");
  assert.equal(h.button.textContent, "install_update");
  const pending = h.button.trigger("click");
  assert.equal(h.button.attributes["aria-busy"], "true");
  assert.equal(h.button.textContent, "update_downloading");
  h.renderProgress({downloaded_bytes: 21_500_000, total_bytes: 43_000_000});
  assert.equal(h.button.textContent, "update_downloading_pct 50");
  assert.equal(h.message.textContent, "update_download_progress_bytes 21.5 43.0");
  h.renderProgress({downloaded_bytes: 43_000_000, total_bytes: 43_000_000, done: true});
  assert.equal(h.button.textContent, "update_verifying");
  assert.equal(h.message.textContent, "update_verify_progress");
  staging.resolve({available_version: "0.0.7-alpha.5", restarting: false});
  await pending;
  assert.equal(h.button.attributes["aria-busy"], "false");
  assert.equal(h.message.textContent, "update_staged 0.0.7-alpha.5");
  // A staged installer keeps the button hidden; progress that arrives
  // outside a staging request is ignored.
  assert.equal(h.button.classList.contains("hidden"), true);
  h.renderProgress({downloaded_bytes: 1, total_bytes: 2});
  assert.equal(h.button.textContent, "install_update");
});

test("offers a restart when the service runs a different build than this window", async () => {
  const calls = [];
  const h = harness(async (name) => { calls.push(name); return {}; });
  h.setDesktopVersion("0.0.7-alpha.4");
  h.renderStatus({state: "active", update_channel: "alpha", version: "0.0.7-alpha.4"});
  assert.equal(h.button.textContent, "check_update");
  h.renderStatus({state: "active", update_channel: "alpha", version: "0.0.7-alpha.5"});
  assert.equal(h.button.textContent, "restart_pulse");
  assert.equal(h.message.textContent, "update_restart_needed 0.0.7-alpha.5");
  await h.button.trigger("click");
  assert.deepEqual(calls, ["RelaunchDesktop"]);
  assert.equal(h.message.textContent, "update_relaunching");
});

test("dev builds never nag about a version mismatch", () => {
  const h = harness(async () => ({}));
  h.setDesktopVersion("0.0.0-dev");
  h.renderStatus({state: "active", update_channel: "alpha", version: "0.0.7-alpha.5"});
  assert.equal(h.button.textContent, "check_update");
});

test("a completed service restart on an old window asks for a relaunch instead of claiming success", async () => {
  const button = element(), message = element(), stream = element();
  const controls = setupUpdates({button, message, stream,
    call: async (name) => name === "CheckUpdate" ? {available_version: "0.0.7-alpha.5"} : name === "StageUpdate" ? {available_version: "0.0.7-alpha.5", restarting: true} : {},
    t: (key, values = {}) => [key, ...Object.values(values)].join(" "), toast: () => {}, onChannelChange: () => {},
    waitForRestart: async () => ({state: "ready", status: {version: "0.0.7-alpha.5", state: "active"}}),
    desktopVersion: "0.0.7-alpha.4",
  });
  controls.renderStatus({state: "active", update_channel: "alpha", version: "0.0.7-alpha.4"});
  await button.trigger("click");
  await button.trigger("click");
  assert.equal(button.textContent, "restart_pulse");
  assert.equal(message.textContent, "update_restart_needed 0.0.7-alpha.5");
  assert.equal(button.classList.contains("hidden"), false);
});
