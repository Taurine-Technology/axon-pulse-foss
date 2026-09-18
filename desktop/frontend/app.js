import {rollingStats, selectInternetTarget, targetSamples} from "./metrics.mjs";
import {setupUpdates} from "./updates.mjs";
import {MIX_FIELDS, HOUSEHOLD_CAP_BYTES, bytesToMB, defaultProfile, describeMix, householdStatusClass, humanizeActivityReason, humanizeStopReason, isSaturationProfile, mixDemand, mixEstimateMB, mixSourceLabel, normalizeMix, phaseDurationSeconds, phaseRateText, profileLabel, validateMix} from "./household.mjs";

const call = (name, ...args) => window.wails.Call.ByName(`main.PulseService.${name}`, ...args);
const $ = (id) => document.getElementById(id);
let status = null;
let samples = [];
let lastSpeedSummary = "";
let lastSpeedResult = null;
let claimRetryTimer = null;
let claimAutoRetries = 0;
let claimAutoResubmit = false;
function clearClaimRetry(){ if(claimRetryTimer){clearInterval(claimRetryTimer);claimRetryTimer=null;} }
const locale = "en";
let nerdMode = false;
try { nerdMode = localStorage.getItem("pulse:nerd-mode") === "1"; } catch {}

const strings = {
  en: {home:"Home",history:"History",speed:"Speed test",settings:"Settings",live:"Live network health",your_connection:"Your connection",latency:"Latency",jitter:"Jitter",loss:"Packet loss",last_minutes:"Last few minutes",local_network:"Local network",internet:"Internet",scheduled_test:"Scheduled speed test",next:"Next",last:"Last",not_scheduled:"Not scheduled",recent:"Recent measurements",on_demand:"On-demand diagnostic",start:"Start",speed_test:"speed test",download:"Download",upload:"Upload",bufferbloat:"Responsiveness",preferences:"Preferences",connection:"Controller connection",data_budget:"Daily speed-test budget",budget_help:"Automatically stops data-heavy tests at the configured limit.",measurement:"Measurement",pause_help:"Basic health checks use only a few kilobytes per minute.",pause:"Pause 1 hour",resume:"Resume",start_login:"Start at login",start_login_help:"Keep Pulse monitoring after you restart this computer.",language:"Language",updates:"Software updates",update_help:"Check Axon's signed release channel.",update_stream:"Update stream",update_stream_help:"Stable is recommended. Beta and Alpha receive new builds earlier and may be less polished.",update_stream_locked:"Set by your administrator for this installation.",stream_main:"Stable",stream_beta:"Beta",stream_alpha:"Alpha",stream_changed:"Now following the {stream} stream",update_check_failed:"Could not check the {stream} stream: {error}",check_update:"Check",install_update:"Download update",what_sent:"What Pulse sends",what_sent_help:"Aggregated network timings, reachability, speed results, device OS/version, the device name once at setup, and optional Wi-Fi signal details. Never browsing content, packet captures, or VPN access.",disconnect:"Disconnect this sensor",disconnect_help:"Removes its local claim. Existing controller history is retained.",background_note:"The measurement service keeps running when this window is closed.",welcome:"Welcome to Axon Pulse",quietly:"Know when your connection lets you down.",privacy_intro:"Pulse measures connection quality in the background. It does not inspect browsing content, capture traffic, or join your organisation's private network.",privacy_one:"Latency, loss, DNS and reachability summaries",privacy_two:"Occasional scheduled speed tests within a data budget",privacy_three:"Local Wi-Fi signal details only when your administrator enables them",controller_url:"Controller URL",claim_token:"Single-use claim token",connect:"Connect securely",token_note:"The token is exchanged once and never stored after pairing. Connecting shares this device's name, OS, and app version with your provider."}
};

Object.assign(strings.en, {
  state_active:"Monitoring",state_offline:"Waiting to upload",state_setup:"Choose a mode",state_paused:"Paused",state_revoked:"Access revoked",state_captive:"Sign-in network",state_service_unavailable:"Service unavailable",
  quality_good:"Connection looks good",quality_warn:"Connection is variable",quality_bad:"Connection needs attention",sign_in_required:"Sign-in may be required",not_connected:"Not connected",budget_used:"{used} of {total} used",grade_summary:"Grade {grade} · {down}↓ {up}↑ Mbps",
  checking:"Checking…",needs_attention:"Needs attention",variable:"Variable",healthy:"Healthy",loading:"Loading…",no_measurements:"No completed measurements yet.",measurement_label:"Measurement",
  connected:"Sensor connected securely",invite_expired:"This invite has expired — ask your provider for a new one.",captive_help:"This network needs you to sign in first. Complete its sign-in page, then try again.",retrying:"Pulse cannot reach the controller. Retrying in {seconds}s…",
  resumed:"Measurement resumed",paused_hour:"Paused for one hour",autostart_on:"Pulse will start at login",autostart_off:"Start at login disabled",disconnect_confirm:"Disconnect this sensor and remove its local credentials?",testing:"Testing",override_confirm:"{message}\n\nRun once anyway and mark the result as overridden?",speed_complete:"Speed test complete",partial:"Partial",speed_incomplete:"Speed test incomplete: {reason}",
  update_apt_managed:"Updates managed by Ubuntu / APT. Running Pulse {version}.",
  update_apt_prerequisite:"Updates require the Pulse APT repository. If you installed a downloaded .deb, set up the repository once below.",
  update_apt_setup:"Set up the Pulse update repository",
  update_apt_channel:"This selects Stable. For Beta or Alpha, add --channel beta or --channel alpha to the sh command.",
  update_apt_help:"Once the repository is configured, use Ubuntu Software Updater or run:",
  update_apt_restart:"After an upgrade the background service restarts itself. If this window is open on the older build, it offers Restart Pulse; closed windows relaunch on their own.",
  update_check_progress:"Checking for updates…",update_downloading:"Downloading…",update_download_progress:"Downloading and verifying the update. This may take a few minutes.",update_failed:"Update failed: {error}",
  update_installed:"Axon Pulse {version} is installed.",update_restart_failed:"The update did not finish. The service is running {version}. Check for updates and try again.",update_restart_unavailable:"Pulse has not reconnected after the update. Close and reopen the app, then check for updates.",
  update_restarting:"Version {version} is verified; the service is restarting.",
  restart_pulse:"Restart Pulse",update_restart_needed:"Version {version} is installed. Restart Pulse to load the new interface.",update_relaunching:"Restarting Pulse…",update_restarting_service:"Restarting service…",update_verifying:"Verifying…",update_downloading_pct:"Downloading {percent}%",update_download_progress_bytes:"Downloaded {downloaded} of {total} MB. Verification follows automatically.",update_verify_progress:"Download complete. Verifying the signature and staging the update.",update_staged:"Version {version} is verified and staged for the signed platform installer.",update_verified:"Update downloaded and checksum verified",update_available:"Version {version} is available.",up_to_date:"Axon Pulse {version} is up to date.",
  local_monitoring:"Pulse is monitoring locally. Nothing will be uploaded.",local_mode_on:"Local-only mode enabled. New measurements stay on this device.",connected_mode_on:"Connected mode enabled.",disconnected_local:"Controller disconnected. Pulse continues locally.",support_exported:"Redacted support bundle exported with private permissions.",will_use_up_to:"Pulse will use up to ",bounded_data:"a bounded amount of data",experience_unavailable:"Not enough phase evidence for an everyday experience score.",experience_capacity:"Experience scores come from household and everyday tests; this saturation test does not update them.",household_summary:"Household {status} · {down}↓ {up}↑ Mbps",mix_saved:"Household mix saved",mix_cleared:"Using the controller or default mix",
  ssid_sharing:"Share Wi-Fi network name",ssid_help:"Off by default. Shared only when your provider requests it; local copies expire within seven days.",ssid_on:"Wi-Fi network name sharing enabled",ssid_off:"Wi-Fi network name sharing disabled and local queued data removed",privacy_three:"Your Wi-Fi network name only if both you and your administrator enable sharing"
});
function t(key, values={}) {
  return (strings[locale][key] || strings.en[key] || key).replace(/\{(\w+)\}/g, (_, name) => String(values[name] ?? ""));
}

function translate() {
  document.documentElement.lang = locale;
  document.querySelectorAll("[data-i18n]").forEach((node) => { node.textContent = strings[locale][node.dataset.i18n] || node.textContent; });
}

function unwrap(value) { return value && typeof value === "object" && "data" in value ? value.data : value; }
function openOnboarding() { const dialog=$("onboarding");if(!dialog.open){dialog.showModal();queueMicrotask(()=>$('use-local').focus());} }
function closeOnboarding() { clearClaimRetry(); const dialog=$("onboarding");if(dialog.open)dialog.close(); }
function number(value, digits=0) { return Number.isFinite(Number(value)) ? Number(value).toFixed(digits) : "—"; }
function speedErrors(result) { return [result?.download?.error, result?.upload?.error, result?.latency?.error, result?.responsiveness?.bidirectional_download?.error, result?.responsiveness?.bidirectional_upload?.error].filter(Boolean); }
function speedValue(result, digits=1) { return result?.error && !Number(result?.bytes_transferred) ? "—" : number(result?.throughput_mbps, digits); }
function speedGrade(result) { return speedErrors(result).length ? t("partial") : (result?.responsiveness?.overall_grade || "—"); }
// speedSummary is the one-line "Last:" text on Home. A household result leads
// with its verdict; a saturation grade must never stand in for everyday use.
function speedSummary(result) {
  const values = {down:speedValue(result.download,0),up:speedValue(result.upload,0)};
  return result?.household ? t("household_summary",{status:result.household.status||"—",...values}) : t("grade_summary",{grade:speedGrade(result),...values});
}

const sampleKey = (sample) => `${sample.target}|${sample.timestamp}`;

// Live ticks omit the profile list and budgets on purpose; when the controller
// configuration or the service build changes, fetch the full status once so
// the picker, schedule and household editor follow without a window reload.
let knownIdentity = null, identityRefresh = null;
function noteServiceIdentity(update) {
  if (!update || (!update.config_version && !update.version)) return;
  const identity = `${update.config_version || ""}|${update.version || ""}`;
  const changed = knownIdentity !== null && identity !== knownIdentity;
  knownIdentity = identity;
  if (!changed || identityRefresh) return;
  identityRefresh = (async () => {
    try { render(unwrap(await call("Status")) || {}); loadHomeSpeed(true); renderSchedule(); } catch (error) { console.error("status refresh failed", error); } finally { identityRefresh = null; }
  })();
}
function render(update={}) {
  if (update.update_progress) { updateControls.renderProgress(update.update_progress); delete update.update_progress; }
  if (update.speed_progress) { const progress = update.speed_progress; delete update.speed_progress; handleSpeedProgress(progress); }
  // Watchdog: a test whose progress stream goes silent (service restart,
  // sleep, crash) must not leave the controls locked on "Testing" forever.
  if ($("speed-ring").disabled && speedLive?.lastAt && Date.now() - speedLive.lastAt > 30000) {
    setSpeedControlsRunning(false);
    $("speed-live-phase").textContent = "Interrupted — the test stopped reporting";
  }
  if (update.samples) {
    // Live updates re-send a trailing window of samples; drop ones already held.
    const seen = new Set(samples.map(sampleKey));
    samples = [...samples, ...update.samples.filter((sample) => !seen.has(sampleKey(sample)))].slice(-600);
  }
  if (update.live) samples = update.live.slice(-600);
  status = {...(status || {}), ...update};
  noteServiceIdentity(update);
  const state = status.state || "service_unavailable";
  $("state-label").textContent = t(`state_${state}`) || state.replaceAll("_", " ");
  $("state-dot").className = state === "active" ? "online" : (["service_unavailable","revoked"].includes(state) ? "error" : "");
  if (state === "setup") openOnboarding();
  // While a mode is already chosen, the dialog (stale claim link, failed
  // mode switch) must offer an explicit way out; Esc alone is not discoverable.
  $("onboarding-close").classList.toggle("hidden", state === "setup");
  const internet = rollingStats(samples, selectInternetTarget(samples));
  const gateway = rollingStats(samples, "gateway");
  const active = internet || gateway;
  if (active) {
    const rtt = active.rtt_ms?.p50 ?? active.rtt_ms;
    $("latency").textContent = number(rtt, 1); $("jitter").textContent = number(active.jitter_ms, 1); $("loss").textContent = number(active.loss_pct, 1);
    const score = active.loss_pct >= 5 || rtt >= 180 ? "bad" : active.loss_pct >= 1 || rtt >= 80 ? "warn" : "good";
    $("quality-pill").className = `pill ${score}`; $("quality-pill").textContent = t(`quality_${score}`);
  }
  $("local-health").textContent = health(gateway); $("internet-health").textContent = status.captive ? t("sign_in_required") : health(internet);
  $("controller-url").textContent = status.controller_url || t("not_connected"); $("sensor-id").textContent = status.sensor_id ? status.sensor_id.slice(0, 12) : "";
  const remaining=Number(status.data_budget_remaining_bytes),limit=Number(status.data_budget_limit_bytes);
  $("budget").textContent = Number.isFinite(limit)&&limit>0 ? t("budget_used",{used:formatBytes(Math.max(0,limit-remaining)),total:formatBytes(limit)}) : formatBytes(remaining);
  const monthlyRemaining=Number(status.monthly_budget_remaining_bytes),monthlyLimit=Number(status.monthly_budget_limit_bytes);
  $("monthly-budget").textContent = Number.isFinite(monthlyLimit)&&monthlyLimit>0 ? t("budget_used",{used:formatBytes(Math.max(0,monthlyLimit-monthlyRemaining)),total:formatBytes(monthlyLimit)}) : formatBytes(monthlyRemaining);
  $("pause-button").textContent = state === "paused" ? strings[locale].resume : strings[locale].pause;
  $("ssid-consent").checked = Boolean(status.ssid_consent);
  $("ssid-consent").disabled = status.mode !== "connected" || state === "service_unavailable";
  $("mode-local").classList.toggle("active", status.mode === "standalone");
  $("mode-connected").classList.toggle("active", status.mode === "connected");
  $("mode-connected").disabled = !status.connected;
  updateControls.renderStatus(status);
  $("update-stream-help").textContent = status.update_channel_locked ? t("update_stream_locked") : t("update_stream_help");
  document.querySelector(".setting.danger").classList.toggle("hidden", !status.connected);
  renderProfilePicker();
  renderHouseholdEditor();
  renderPrediction();
  renderSchedule();
  drawChart();
}

// Live updates carry no schedule fields, so the startup-time next_speed_test
// survives render()'s merge forever; a displayed slot in the past means the
// daemon has moved on and the full Status must be re-fetched (throttled so
// the ~1-2s live stream cannot stampede the IPC socket).
let scheduleRefreshAt = 0;
function renderSchedule() {
  const localeCode = "en-ZA";
  const options = {weekday:"short",hour:"2-digit",minute:"2-digit"};
  const next = new Date(status?.next_speed_test || 0);
  const last = new Date(status?.last_speed_test || 0);
  if (next.getFullYear() > 2000 && next.getTime() < Date.now() - 60000 && Date.now() - scheduleRefreshAt > 60000) {
    scheduleRefreshAt = Date.now();
    // Full Status is authoritative for the schedule: pre-clear both fields so
    // an omitted (omitzero) timestamp falls back to "not scheduled" instead of
    // the merge resurrecting the stale one.
    call("Status").then((full) => { const fresh = unwrap(full); if (fresh) render({next_speed_test:"", last_speed_test:"", ...fresh}); }).catch(() => {});
  }
  const minutes = Math.max(0, Math.ceil((next-Date.now())/60000));
  const countdown = minutes < 60 ? `${minutes}m` : `${Math.floor(minutes/60)}h ${minutes%60}m`;
  $("speed-schedule").textContent = next.getFullYear() > 2000 ? `${strings[locale].next}: ${countdown} · ${next.toLocaleString(localeCode, options)}` : strings[locale].not_scheduled;
  $("last-speed").textContent = lastSpeedSummary || (last.getFullYear() > 2000 ? `${strings[locale].last}: ${last.toLocaleString(localeCode, options)}` : "");
}

// homeOnly refreshes the Home experience cards from the latest eligible
// result without replacing whatever the Speed screen is currently showing
// (a saturation run the user just started must stay visible).
async function loadHomeSpeed(homeOnly = false) {
  try {
    const rows = unwrap(await call("SpeedTestHistory", 168)) || [];
    const row = [...rows].reverse().find((entry) => entry.kind === "speed_test" && entry.data?.experience_eligible !== false);
    if (!row) return;
    const result = row.data || {};
    if (homeOnly) { renderExperience(result.experience, result.ts, Boolean(result.household)); return; }
    lastSpeedResult = result;
    lastSpeedSummary = speedSummary(result);
    renderSpeedResult(result);
    renderSchedule();
  } catch {}
}

// loadLatestSpeedResult shows the newest stored result on the Speed screen,
// whatever its eligibility: a scheduled test that just finished is what the
// user wants to see, and Home keeps its own eligible-only view.
async function loadLatestSpeedResult() {
  try {
    const rows = unwrap(await call("SpeedTestHistory", 168)) || [];
    const row = [...rows].reverse().find((entry) => entry.kind === "speed_test");
    if (!row) return;
    lastSpeedResult = row.data || {};
    lastSpeedSummary = speedSummary(lastSpeedResult);
    renderSpeedResult(lastSpeedResult);
    renderSchedule();
    loadHomeSpeed(true);
  } catch {}
}

// The picker lists whatever profiles the service offers; it is rebuilt only
// when that list changes so status ticks never reset the user's choice.
let profilePickerKey = null;
function renderProfilePicker() {
  const names = (status?.speed_profiles || []).map((profile) => profile.name);
  const key = names.join(",");
  if (key === profilePickerKey) return;
  profilePickerKey = key;
  const select = $("speed-profile");
  const previous = select.value;
  select.innerHTML = names.map((name) => `<option value="${safe(name)}">${safe(profileLabel(name))}</option>`).join("");
  select.value = names.includes(previous) ? previous : defaultProfile(names);
}

function selectedProfile() {
  const name = $("speed-profile").value;
  return name ? (status?.speed_profiles || []).find((profile) => profile.name === name) || null : null;
}

function renderPrediction() {
  const profile = selectedProfile();
  // A Start-button test uses the device-owned envelope, which can exceed the
  // remotely scheduled size and even the remaining ledger; a user is warned
  // about budget overage but never stopped.
  const localEnvelope = Number(status?.manual_test_envelope_bytes?.[profile?.name] || 0);
  const bytes = localEnvelope || Number(profile?.max_download_bytes || 0) + Number(profile?.max_upload_bytes || 0);
  const dailyRemaining = Number(status?.data_budget_remaining_bytes);
  const monthlyRemaining = Number(status?.monthly_budget_remaining_bytes);
  const overBudget = (Number.isFinite(dailyRemaining) && bytes > dailyRemaining) || (Number.isFinite(monthlyRemaining) && bytes > monthlyRemaining);
  if (profile?.name === "household") {
    // A paced household run moves only what its activities demand; the cap is
    // a ceiling the user should know about, not the expected cost.
    const cap = Math.round(bytesToMB(bytes || HOUSEHOLD_CAP_BYTES));
    $("speed-note-lead").textContent = "Your household mix needs about ";
    $("predicted-data").textContent = `${Math.round(mixEstimateMB(status?.household_mix))} MB`;
    $("speed-note-tail").textContent = ` per test — usually far less than the ${cap} MB cap — and checks each activity's delivery while they all run together.`;
  } else {
    $("speed-note-lead").textContent = "Pulse sizes the test to your connection and will use up to ";
    $("predicted-data").textContent = bytes ? formatBytes(bytes) : t("bounded_data");
    $("speed-note-tail").textContent = isSaturationProfile(profile?.name) ? " while driving the line to its limit in each direction." : " and measure latency while the link is busy.";
  }
  $("budget-warning").textContent = bytes && overBudget ? " This may exceed today's remaining data budget." : "";
}

// Household mix editor. Inputs follow the effective mix from status until the
// user touches them; a dirty editor is never overwritten by a status tick.
const householdEditor = {dirty: false, applied: ""};
// Field labels are static catalog strings, so no escaping is needed here.
$("household-fields").innerHTML = MIX_FIELDS.map((field) =>
  `<label class="hh-field"><span>${field.label}</span><span class="stepper"><button type="button" data-field="${field.key}" data-delta="-1" aria-label="Fewer ${field.label}">−</button>` +
  `<input type="number" inputmode="numeric" min="0" max="4" step="1" value="0" data-field="${field.key}" aria-label="${field.label}" /><button type="button" data-field="${field.key}" data-delta="1" aria-label="More ${field.label}">+</button></span></label>`).join("") +
  `<label class="hh-field"><span>Browsing</span><label class="switch"><input id="household-browsing" type="checkbox" aria-label="Include browsing" /><span></span></label></label>`;
function readEditorMix() {
  const mix = {browsing: $("household-browsing").checked};
  document.querySelectorAll("#household-fields input[type=number]").forEach((input) => { mix[input.dataset.field] = input.value === "" ? NaN : Number(input.value); });
  return mix;
}
function writeEditorMix(mix) {
  const m = normalizeMix(mix);
  document.querySelectorAll("#household-fields input[type=number]").forEach((input) => { input.value = String(m[input.dataset.field]); });
  $("household-browsing").checked = m.browsing;
  renderHouseholdDemand();
}
function renderHouseholdDemand() {
  const mix = readEditorMix();
  const check = validateMix(mix);
  $("household-error").textContent = check.message;
  $("household-save").disabled = !check.ok;
  if (!check.ok) { $("household-demand").textContent = ""; return; }
  const demand = mixDemand(mix);
  $("household-demand").textContent = `${describeMix(mix)} · demand ${demand.total.toFixed(1)} Mbps (${demand.down.toFixed(1)}↓ ${demand.up.toFixed(1)}↑) · about ${Math.round(mixEstimateMB(mix))} MB per test`;
}
function renderHouseholdEditor() {
  const unavailable = status?.household_available === false;
  $("household-unavailable").classList.toggle("hidden", !unavailable);
  $("household-editor").classList.toggle("hidden", unavailable);
  $("household-mix-source").textContent = unavailable ? "" : mixSourceLabel(status?.household_mix_source);
  $("household-reset").disabled = status?.household_mix_source !== "local";
  const mix = status?.household_mix;
  if (!mix || householdEditor.dirty) return;
  const key = JSON.stringify(normalizeMix(mix));
  if (key === householdEditor.applied) return;
  householdEditor.applied = key;
  writeEditorMix(mix);
}

let speedLive = null;
const phaseLabels = {
  traffic_check: "Checking for other traffic", baseline: "Measuring idle latency",
  download_warmup: "Calibrating download", upload_warmup: "Calibrating upload",
  download: "Download + latency under load", upload: "Upload + latency under load",
  bidirectional: "Both directions at once", cooldown: "Measuring recovery", done: "Complete", aborted: "Stopped",
  reference: "Reference stream", household: "Household mix",
};
const regionReasons = {
  latency_only_phase: "Latency only — no bulk transfer", calibration_excluded: "Calibration (not scored)",
  insufficient_complete_buckets: "Too short to judge stability", throughput_did_not_stabilize: "Throughput did not stabilize",
  byte_or_time_budget_exhausted: "Budget spent before this phase", paced_scenario: "Paced activities (stability not applicable)",
  neither_direction_stable: "Neither direction stabilized", download_not_stable: "Download did not stabilize",
  upload_not_stable: "Upload did not stabilize", directions_not_overlapping: "Stable windows did not overlap",
};
const confidenceNotes = {
  cross_traffic_detected: "other traffic during the test",
  too_few_latency_probes: "many latency probes failed", some_latency_probes_missing: "some latency probes failed",
  fresh_path_probes_unreliable: "new-connection probes were unreliable",
  network_changed_during_test: "the network changed mid-test", ip_family_changed_during_test: "the connection path changed mid-test",
};
function humanizeReason(reason) {
  if (confidenceNotes[reason]) return confidenceNotes[reason];
  const budget = reason.match(/^(download|upload|bidirectional)_budget_limited$/);
  if (budget) return `${budget[1]} data budget ran out before stability`;
  return String(reason).replaceAll("_", " ");
}
const exclusionText = {
  cross_traffic: "other traffic was using the connection", budget_capped: "the data budget capped it",
  profile_excluded: "saturation tests don't score everyday experience", required_phase_error: "a phase failed",
  throughput_unavailable: "throughput evidence was missing", latency_evidence_unavailable: "latency evidence was missing",
  experience_unavailable: "experience evidence was missing", low_confidence: "measurement confidence was too low",
};

// setSpeedControlsRunning locks the Start ring and the test-type picker for
// the duration of a run — including runs started from the tray, CLI, or
// scheduler — so a running test can be neither restarted nor retargeted.
function setSpeedControlsRunning(running) {
  const ring = $("speed-ring");
  ring.disabled = running;
  $("speed-profile").disabled = running;
  ring.classList.toggle("running", running);
  $("speed-action").textContent = running ? t("testing") : strings[locale].start;
}

function handleSpeedProgress(p) {
  if (!p || !p.measurement_id) return;
  if (!speedLive || speedLive.id !== p.measurement_id) {
    // A test started elsewhere (tray, CLI, scheduler): clear the previous
    // result's panels just like a Start-button run does.
    if (!p.done && speedLive?.id !== "preflight") resetSpeedDisplays();
    speedLive = {id: p.measurement_id, points: []};
  }
  speedLive.lastAt = Date.now();
  speedLive.points.push({t: p.elapsed_ms || 0, down: p.download_mbps || 0, up: p.upload_mbps || 0, rtt: p.rtt_ms || 0, phase: p.phase});
  if (speedLive.points.length > 900) speedLive.points.shift();
  $("speed-live").classList.remove("hidden");
  $("speed-live-phase").textContent = phaseLabels[p.phase] || p.phase || "";
  $("speed-live-elapsed").textContent = `${((p.elapsed_ms || 0) / 1000).toFixed(1)} s`;
  $("speed-live-rtt").textContent = p.rtt_ms ? `${number(p.rtt_ms, 0)} ms` : "—";
  if (p.done) {
    setSpeedControlsRunning(false);
    loadSpeedHistory();
    loadLatestSpeedResult();
  } else {
    if (!$("speed-ring").disabled) setSpeedControlsRunning(true);
    if (p.download_mbps) $("speed-down").textContent = number(p.download_mbps, 1);
    if (p.upload_mbps) $("speed-up").textContent = number(p.upload_mbps, 1);
  }
  drawSpeedLive();
}

function drawSpeedLive() {
  if (document.hidden || !speedLive || !speedLive.points.length || !$("screen-speed").classList.contains("active")) return;
  const canvas = $("speed-live-chart"), rect = canvas.getBoundingClientRect();
  if (!rect.width) return;
  const scale = window.devicePixelRatio || 1;
  canvas.width = Math.max(1, rect.width * scale); canvas.height = Math.max(1, rect.height * scale);
  const ctx = canvas.getContext("2d"); ctx.scale(scale, scale);
  const w = rect.width, h = rect.height, pad = 8;
  const pts = speedLive.points;
  const tMax = Math.max(pts[pts.length - 1].t, 5000);
  const rateMax = Math.max(10, ...pts.map((q) => Math.max(q.down, q.up))) * 1.15;
  const rttMax = Math.max(20, ...pts.map((q) => q.rtt)) * 1.3;
  const x = (t) => pad + (w - 2 * pad) * t / tMax;
  const yRate = (v) => h - pad - (h - 2 * pad) * v / rateMax;
  const yRtt = (v) => h - pad - (h - 2 * pad) * v / rttMax;
  const vars = getComputedStyle($("speed-live"));
  const cDown = vars.getPropertyValue("--chart-down").trim() || "#7e7cfb";
  const cUp = vars.getPropertyValue("--chart-up").trim() || "#2fc27a";
  const cRtt = vars.getPropertyValue("--chart-rtt").trim() || "#f5b942";
  ctx.clearRect(0, 0, w, h);
  ctx.strokeStyle = "rgba(128,128,160,.3)"; ctx.lineWidth = 1;
  for (let i = 1; i < pts.length; i++) {
    if (pts[i].phase !== pts[i - 1].phase) { const px = x(pts[i].t); ctx.beginPath(); ctx.moveTo(px, pad); ctx.lineTo(px, h - pad); ctx.stroke(); }
  }
  const line = (key, color, yFn, fill) => {
    ctx.beginPath();
    let started = false;
    for (const q of pts) {
      if (key === "rtt" && !q.rtt) continue;
      const px = x(q.t), py = yFn(q[key]);
      if (!started) { ctx.moveTo(px, py); started = true; } else ctx.lineTo(px, py);
    }
    if (!started) return;
    ctx.strokeStyle = color; ctx.lineWidth = key === "rtt" ? 1.5 : 2; ctx.stroke();
    if (fill) { ctx.lineTo(x(pts[pts.length - 1].t), h - pad); ctx.lineTo(x(pts[0].t), h - pad); ctx.closePath(); ctx.fillStyle = color + "22"; ctx.fill(); }
  };
  line("down", cDown, yRate, true);
  line("up", cUp, yRate, false);
  line("rtt", cRtt, yRtt, false);
  ctx.fillStyle = "rgba(128,128,160,.9)"; ctx.font = "10px 'IBM Plex Mono', ui-monospace, monospace";
  ctx.fillText(`${Math.round(rateMax)} Mbps`, pad + 3, pad + 10);
  ctx.textAlign = "right"; ctx.fillText(`${Math.round(rttMax)} ms`, w - pad - 3, pad + 10); ctx.textAlign = "left";
}

// speedVerdict turns a result's grade + per-app scores into one plain
// sentence: what the grade means, why, and what the connection is good for.
function speedVerdict(result) {
  const r = result.responsiveness || {};
  const grade = r.overall_grade || "";
  const bloat = Math.round(Math.max(r.download_bloat_p90_ms || 0, r.upload_bloat_p90_ms || 0));
  const bidiBloat = Math.round(r.bidirectional_bloat_p90_ms || 0);
  const driver = r.primary_driver === "upload" ? "uploading" : r.primary_driver === "download" ? "downloading" : "busy";
  // The headline grade covers one-way load only; video calls and gaming are
  // judged on the simultaneous send+receive phase, which can be much worse.
  // The copy must attribute each verdict to the load that produced it.
  let why = "";
  if (grade === "A+" || grade === "A") why = `Grade ${grade}: latency stays low under one-way load.`;
  else if (grade === "B") why = `Grade B: latency rises a little (~${bloat} ms) under one-way load.`;
  else if (grade) why = `Grade ${grade}: latency jumps ~${bloat} ms while the line is ${driver} — real-time apps feel this as lag (bufferbloat).`;
  const home = result.experience?.home || {};
  const labels = {browsing: "browsing", streaming: "streaming", video_calls: "video calls", gaming: "gaming"};
  const good = [], rough = [];
  for (const [key, label] of Object.entries(labels)) {
    const app = home[key];
    if (!app || app.available === false) continue;
    if (app.rating === "bad") rough.push(label); else good.push(label);
  }
  const list = (items) => items.length > 1 ? `${items.slice(0, -1).join(", ")} and ${items.at(-1)}` : items[0];
  const roughCause = bidiBloat >= 60 ? ` — latency jumped ~${bidiBloat} ms when sending and receiving at once, which is exactly what they feel` : " in this run's combined send-and-receive phase";
  let fit = "";
  if (good.length && rough.length) fit = ` Good for ${list(good)}, but ${list(rough)} scored poorly${roughCause}.`;
  else if (good.length) fit = ` Good for ${list(good)}.`;
  else if (rough.length) fit = ` ${list(rough)[0].toUpperCase()}${list(rough).slice(1)} scored poorly${roughCause}.`;
  return (why + fit).trim();
}

// resetSpeedDisplays clears every panel of the previous result so a starting
// test never shows stale numbers under a "Testing" ring.
function resetSpeedDisplays() {
  $("speed-down").textContent = "—";
  $("speed-up").textContent = "—";
  $("speed-grade").textContent = "—";
  $("speed-confidence").className = "confidence neutral";
  $("speed-confidence").textContent = "Measurement confidence: waiting";
  $("speed-verdict").classList.add("hidden");
  $("saturation-note").classList.add("hidden");
  renderHouseholdResult(null);
  $("measurement-id").textContent = "";
  $("persistent-p95").textContent = "—";
  $("fresh-p95").textContent = "—";
  $("test-data-used").textContent = "—";
  $("phase-list").innerHTML = `<div class="empty">${safe("Test running…")}</div>`;
  $("confidence-reasons").textContent = "";
}

const speedHistory = {rows: [], page: 0, selected: "", perPage: 5, limit: 30};

async function loadSpeedHistory() {
  try {
    const rows = unwrap(await call("SpeedTestHistory", 168)) || [];
    let tests = rows.filter((row) => row.kind === "speed_test" && row.data).reverse();
    // A test that just finished reaches durable history on the next minute
    // flush; surface it immediately from memory so the list never lags.
    if (lastSpeedResult?.measurement_id && !tests.some((row) => row.data.measurement_id === lastSpeedResult.measurement_id)) {
      tests.unshift({kind: "speed_test", timestamp: lastSpeedResult.ts || Math.floor(Date.now() / 1000), data: lastSpeedResult});
    }
    speedHistory.rows = tests.slice(0, speedHistory.limit);
    speedHistory.page = Math.min(speedHistory.page, Math.max(0, Math.ceil(speedHistory.rows.length / speedHistory.perPage) - 1));
    renderSpeedHistory();
  } catch {}
}

function renderSpeedHistory() {
  const list = $("speed-history-list");
  const rows = speedHistory.rows;
  $("speed-history-count").textContent = rows.length ? `last 7 days` : "";
  if (!rows.length) {
    list.innerHTML = `<div class="empty">No speed tests in the last 7 days.</div>`;
    $("speed-history-pager").classList.add("hidden");
    return;
  }
  const pages = Math.ceil(rows.length / speedHistory.perPage);
  const start = speedHistory.page * speedHistory.perPage;
  const visible = rows.slice(start, start + speedHistory.perPage);
  list.innerHTML = visible.map((row, index) => {
    const data = row.data;
    const when = new Date(row.timestamp * 1000).toLocaleString([], {weekday: "short", hour: "2-digit", minute: "2-digit"});
    // A household row leads with its verdict: "Everyday" would misdescribe a
    // paced mix, and a saturation row must keep its own label.
    const household = data.household;
    const profile = household ? `Household <span class="rating ${householdStatusClass(household.status)}">${safe(household.status || "—")}</span>` : safe(profileLabel(data.profile));
    const excluded = data.experience_eligible === false ? `<span class="row-note">excluded</span>` : "";
    const selected = data.measurement_id && data.measurement_id === speedHistory.selected ? " selected" : "";
    return `<button type="button" class="speed-history-row${selected}" data-index="${start + index}">` +
      `<span class="row-time">${safe(when)}</span><span>${profile} ${excluded}</span>` +
      `<span>${speedValue(data.download, 0)}↓ ${speedValue(data.upload, 0)}↑ Mbps</span>` +
      `<span class="row-grade">${safe(data.responsiveness?.overall_grade || "—")}</span></button>`;
  }).join("");
  const pager = $("speed-history-pager");
  pager.classList.toggle("hidden", pages <= 1);
  $("speed-history-page").textContent = `${start + 1}–${Math.min(start + speedHistory.perPage, rows.length)} of ${rows.length}`;
  $("speed-history-prev").disabled = speedHistory.page === 0;
  $("speed-history-next").disabled = speedHistory.page >= pages - 1;
}

function renderSpeedResult(result, fromHistory = false) {
  if (!result) return;
  if (!fromHistory) lastSpeedResult = result;
  speedHistory.selected = result.measurement_id || "";
  document.querySelectorAll(".speed-history-row").forEach((row) => {
    row.classList.toggle("selected", speedHistory.rows[Number(row.dataset.index)]?.data?.measurement_id === speedHistory.selected);
  });
  // A household result speaks through its own block; the grade-led verdict
  // describes one-way saturation load and would contradict a paced mix.
  const verdict = result.household ? "" : speedVerdict(result);
  $("speed-verdict").textContent = verdict;
  $("speed-verdict").classList.toggle("hidden", !verdict);
  $("saturation-note").classList.toggle("hidden", !isSaturationProfile(result.profile));
  renderHouseholdResult(result.household);
  $("speed-down").textContent=speedValue(result.download,1);
  $("speed-up").textContent=speedValue(result.upload,1);
  $("speed-grade").textContent=speedGrade(result);
  const confidence = result.measurement_confidence || {level:result.responsiveness?.confidence_level,reasons:result.responsiveness?.confidence_reasons || []};
  $("speed-confidence").className = `confidence ${confidence.level || "neutral"}`;
  $("speed-confidence").textContent = `Measurement confidence: ${confidence.level || "unknown"}` +
    (result.experience_eligible === false ? ` · not used on Home Screen as ${exclusionText[result.exclusion_reason] || result.exclusion_reason || "it was excluded"}` : "");
  $("measurement-id").textContent = result.measurement_id || "";
  $("persistent-p95").textContent = number(result.paths?.persistent?.p95_ms,1);
  $("fresh-p95").textContent = number(result.paths?.fresh?.p95_ms,1);
  $("test-data-used").textContent = number(Number(result.budget?.used_bytes || 0)/1048576,1);
  $("confidence-reasons").textContent = (confidence.reasons || []).length ? `Confidence notes: ${confidence.reasons.map(humanizeReason).join("; ")}.` : "No measurement-confidence warnings.";
  const phases = Object.entries(result.phases || {});
  $("phase-list").innerHTML = phases.length ? phases.map(([name,phase]) => {
    // Duration is the phase's own window; a phase without a stable region has
    // no stable throughput to report, only the plain observed transfer rate.
    const region=phase.stable_region || {}, duration=phaseDurationSeconds(phase);
    return `<div class="phase-row"><strong>${safe(name.replaceAll("_"," "))}</strong><span>${safe(region.stable?`stable buckets ${region.start_bucket}–${region.end_bucket}`:(regionReasons[region.reason]||(region.reason||"not stable").replaceAll("_"," ")))}</span><span>${safe(phaseRateText(phase))}</span><span>${number(duration,1)} s</span></div>`;
  }).join("") : `<div class="empty">No phase evidence was returned.</div>`;
  if (fromHistory) return;
  if (result.experience_eligible === false) {
    resetExperience(t("experience_capacity"));
  } else {
    renderExperience(result.experience, result.ts, Boolean(result.household));
  }
}

// renderHouseholdResult shows the household verdict as the weakest activity,
// never an average, with each activity's demand and delivery beside it.
function renderHouseholdResult(hh) {
  const block = $("household-result");
  block.classList.toggle("hidden", !hh);
  if (!hh) { block.innerHTML = ""; return; }
  const mbps = (down, up) => `${number(down,1)}↓ ${number(up,1)}↑`;
  const facts = [];
  if (hh.reference) facts.push(`${safe(hh.reference.label || "Reference stream")} alone: <span class="rating ${safe(hh.reference.rating)}">${safe(hh.reference.rating)} · ${number(hh.reference.score,0)}</span>`);
  facts.push(`stopped: ${safe(humanizeStopReason(hh.stop_reason))}`);
  facts.push(`observed ${number(Number(hh.window?.observed_ms || 0)/1000,0)} s`);
  facts.push(`${number(bytesToMB(hh.ledger?.used_bytes),0)} of ${number(bytesToMB(hh.ledger?.cap_bytes),0)} MB`);
  const rows = (hh.activities || []).map((a) => {
    const label = `${safe(a.label || a.key)}${a.instance > 1 ? ` <span class="mono">#${Number(a.instance)}</span>` : ""}` +
      (a.estimate ? ` <span class="tag" title="Measured over HTTPS; not UDP media quality">estimate</span>` : "");
    const score = a.rating === "unavailable" || a.available === false ? `<span class="rating unavailable">unavailable</span>` : `<span class="rating ${safe(a.rating)}">${safe(a.rating)} · ${number(a.score,0)}</span>`;
    return `<tr><td>${label}</td><td>${mbps(a.demand_down_mbps,a.demand_up_mbps)}</td><td>${mbps(a.achieved_down_mbps,a.achieved_up_mbps)}</td>` +
      `<td>${number(a.p90_delay_ms,0)} / ${number(a.p95_delay_ms,0)}</td><td>${number(a.late_pct,1)}</td><td>${number(a.loss_pct,1)}</td><td>${score}</td><td class="why">${safe((a.reasons || []).map(humanizeActivityReason).join(", "))}</td></tr>`;
  }).join("");
  block.innerHTML = `<div class="hh-head"><div><h3>Household mix</h3><p>${safe(describeMix(hh.mix))}</p><p class="mono">mix: ${safe(hh.mix_source || "default")} · ${safe(hh.mix_revision || "")}</p></div>` +
    `<span class="pill ${householdStatusClass(hh.status)}">${safe(hh.status || "—")}</span></div>` +
    `<p class="hh-summary">${safe(hh.summary || "")}</p><p class="hh-meta">${facts.join(" · ")}</p>` +
    (rows ? `<div class="hh-table-wrap"><table class="hh-table"><thead><tr><th>Activity</th><th>Demand Mbps</th><th>Achieved</th><th title="Typical (p90) and tail (p95) delivery delay">p90 / p95 ms</th><th>Late %</th><th>Loss %</th><th>Score</th><th>Why</th></tr></thead><tbody>${rows}</tbody></table></div>` : "");
}

document.querySelectorAll(".experience-card small").forEach((node) => { node.dataset.initial = node.textContent; });

function resetExperience(message) {
  $("experience-score").textContent = "—";
  $("experience-summary").textContent = message;
  document.querySelectorAll(".experience-card").forEach((card) => {
    card.classList.remove("good","average","bad");
    card.querySelector("strong").textContent = "—";
    const small = card.querySelector("small");
    small.textContent = small.dataset.initial;
  });
}

// Card copy must state the verdict, not the category's aspiration — "Bad"
// above "conversation should stay clear" reads as a contradiction.
const experienceCopy = {
  browsing: {
    good: "Pages and requests should feel responsive.",
    average: "Pages should load fine, with occasional slow starts.",
    bad: "Pages may be slow to start, even when the line is idle.",
  },
  streaming: {
    good: "Enough headroom for sustained video without stalls.",
    average: "Video should mostly play fine, with occasional quality drops.",
    bad: "Expect buffering or quality drops during sustained video.",
  },
  video_calls: {
    good: "Calls should stay clear while sending and receiving together.",
    average: "Calls should mostly hold up, with occasional glitches.",
    bad: "Calls may stutter or freeze while the line is busy.",
  },
  gaming: {
    good: "Controls should stay responsive while the connection is busy.",
    average: "Mostly responsive, with occasional lag spikes under load.",
    bad: "Expect lag while the connection is busy.",
  },
};

function renderExperience(experience, timestamp, household = false) {
  if (!experience?.home) { resetExperience(t("experience_unavailable")); return; }
  const when = timestamp ? new Date(timestamp * 1000).toLocaleString([], {weekday: "short", hour: "2-digit", minute: "2-digit"}) : "";
  $("experience-score").textContent = experience.available === false ? "—" : `${number(experience.overall_score,0)}%`;
  const source = household ? "household test" : "speed test";
  $("experience-summary").textContent = experience.available === false ? "Not enough phase evidence for an everyday experience score." : `From your ${source}${when ? ` (${when})` : ""} · overall: ${experience.rating || "unknown"}${household ? " (weakest activity)" : ""}. Live health is shown above.`;
  document.querySelectorAll(".experience-card").forEach((card) => {
    const app = experience.home[card.dataset.app];
    if (!app) return;
    card.classList.remove("good","average","bad");
    const rating = app.rating || "average";
    if (app.available !== false) card.classList.add(rating);
    const reasons = app.unavailable_reasons || [];
    const notInMix = household && reasons.includes("not_in_mix");
    const baselineStarved = reasons.includes("insufficient_baseline_samples");
    const starved = reasons.includes("insufficient_load_samples");
    card.querySelector("strong").textContent = app.available === false ? (notInMix ? "Not in your mix" : baselineStarved ? "Needs more measurements" : starved ? "Needs a longer test" : "Unavailable") : `${rating} · ${number(app.score,0)}%`;
    const outcome = app.available === false ? "" : experienceCopy[card.dataset.app]?.[rating];
    const small = card.querySelector("small");
    small.textContent = app.available === false && notInMix ? "Add it to your household mix on the Speed test screen to have it tested." : app.available === false && baselineStarved ? `Too few idle responses to judge browsing. Run another ${household ? "household" : "everyday"} test.` : app.available === false && starved ? `The scheduled test finished before this could be judged. Run ${household ? "another household test" : "a longer test"} for a full assessment.` : (outcome || app.explanation || small.dataset.initial);
  });
}

function health(item) {
  if (!item) return t("checking");
  const rtt = Number(item.rtt_ms?.p50 ?? item.rtt_ms);
  if (Number(item.loss_pct) >= 5 || rtt >= 180) return t("needs_attention");
  if (Number(item.loss_pct) >= 1 || rtt >= 80) return t("variable");
  return t("healthy");
}
function formatBytes(value) { if (!Number.isFinite(Number(value))) return "—"; return `${(Number(value)/1048576).toFixed(0)} MB`; }

function drawChart() {
  if (document.hidden) return;
  const canvas = $("latency-chart"), rect = canvas.getBoundingClientRect(), scale = window.devicePixelRatio || 1;
  canvas.width = Math.max(1, rect.width * scale); canvas.height = Math.max(1, rect.height * scale);
  const ctx = canvas.getContext("2d"); ctx.scale(scale, scale); const w=rect.width,h=rect.height,p=18;
  const tokens = getComputedStyle(document.documentElement);
  ctx.clearRect(0,0,w,h); ctx.strokeStyle=tokens.getPropertyValue("--line"); ctx.lineWidth=1;
  for(let i=1;i<4;i++){const y=(h-p)*i/4;ctx.beginPath();ctx.moveTo(p,y);ctx.lineTo(w,y);ctx.stroke();}
  const target=selectInternetTarget(samples);
  const values=targetSamples(samples,target,10*60_000).filter((s)=>s.success===true).slice(-90).map((s)=>Number(s.rtt_ms?.p50 ?? s.rtt_ms)).filter(Number.isFinite);
  if(values.length<2)return; const high=Math.max(50,...values)*1.2;
  ctx.beginPath(); values.forEach((v,i)=>{const x=p+i*(w-2*p)/(values.length-1),y=h-p-v*(h-2*p)/high;i?ctx.lineTo(x,y):ctx.moveTo(x,y)});ctx.strokeStyle=tokens.getPropertyValue("--chart-line").trim()||"#7e7cfb";ctx.lineWidth=2.2;ctx.lineJoin="round";ctx.stroke();
}

async function loadHistory() {
  const list=$("history-list"); list.innerHTML=`<div class="empty">${safe(t("loading"))}</div>`;
  try { const rows=unwrap(await call("History", Number($("history-range").value))) || []; list.innerHTML=rows.length ? rows.slice(-500).reverse().map(historyRow).join("") : `<div class="empty">${safe(t("no_measurements"))}</div>`; }
  catch(error){ list.innerHTML=`<div class="empty">${safe(error.message || error)}</div>`; }
}
function historyRow(row) { const data=row.data||{}, detail=row.kind==="speed_test"?`${speedValue(data.download,1)} ↓ / ${speedValue(data.upload,1)} ↑ Mbps`:data.target||data.domain||data.type||t("measurement_label"); const quality=row.kind==="speed_test"?speedGrade(data):number(data.rtt_ms?.p50,1); return `<div class="history-row"><span class="time">${new Date(row.timestamp*1000).toLocaleString()}</span><span class="kind">${safe(row.kind.replaceAll("_"," "))}</span><span class="detail">${safe(detail)}</span><strong>${safe(quality)}</strong></div>`; }
function safe(value){ const node=document.createElement("span");node.textContent=String(value??"");return node.innerHTML; }
function toast(message){const node=$("toast");node.textContent=message;node.classList.add("show");setTimeout(()=>node.classList.remove("show"),2600);}

document.querySelectorAll(".nav").forEach((button)=>button.addEventListener("click",()=>{document.querySelectorAll(".nav,.screen").forEach((n)=>n.classList.remove("active"));button.classList.add("active");$(`screen-${button.dataset.screen}`).classList.add("active");if(button.dataset.screen==="history")loadHistory();if(button.dataset.screen==="home")drawChart();if(button.dataset.screen==="speed"){loadSpeedHistory();drawSpeedLive();}}));
$("speed-history-list").addEventListener("click",(event)=>{
  const row=event.target.closest(".speed-history-row");
  if(!row)return;
  const entry=speedHistory.rows[Number(row.dataset.index)];
  if(entry?.data)renderSpeedResult(entry.data,true);
});
$("speed-history-prev").addEventListener("click",()=>{if(speedHistory.page>0){speedHistory.page--;renderSpeedHistory();}});
$("speed-history-next").addEventListener("click",()=>{speedHistory.page++;renderSpeedHistory();});
$("history-range").addEventListener("change",loadHistory);
$("speed-profile").addEventListener("change",renderPrediction);
$("use-local").addEventListener("click",async()=>{
  // Stop any pending claim auto-retry before awaiting, so it cannot
  // requestSubmit() the still-open dialog while SetMode is in flight.
  clearClaimRetry();
  const button=$("use-local");button.disabled=true;
  try { render(unwrap(await call("SetMode","standalone"))||{});closeOnboarding();toast(t("local_monitoring")); }
  catch(error){$("claim-error").textContent=error.message||String(error);}
  finally{button.disabled=false;}
});
$("claim-form").addEventListener("submit",async(event)=>{
  event.preventDefault();
  clearClaimRetry();
  const autoRetry=claimAutoResubmit;claimAutoResubmit=false;
  if(!autoRetry)claimAutoRetries=0;
  const button=$("claim-button"),token=$("claim-token");
  button.disabled=true;$("claim-error").textContent="";
  try {
    const result=unwrap(await call("Connect",$("claim-url").value.trim(),token.value.trim()));
    token.value="";closeOnboarding();render(result||{});toast(t("connected"));
  } catch(error) {
    const message=error.message||String(error);
    if (/expired|invalid_claim_token/i.test(message)) {
      token.value="";$("claim-error").textContent=t("invite_expired");
    } else if (/already[_ ]claimed|disconnect this sensor/i.test(message)) {
      token.value="";$("claim-error").textContent=message;
    } else if (status?.captive) {
      $("claim-error").textContent=t("captive_help");
    } else if (/HTTP 4\d\d|already_claimed|disconnect this sensor/i.test(message) || claimAutoRetries>=2) {
      // Controller rejections cannot heal on their own; only transient transport failures earn bounded retries.
      $("claim-error").textContent=message;
    } else {
      claimAutoRetries++;
      let remaining=10;
      $("claim-error").textContent=t("retrying",{seconds:remaining});
      claimRetryTimer=setInterval(()=>{remaining--;$("claim-error").textContent=t("retrying",{seconds:remaining});if(remaining<=0){clearClaimRetry();if($("onboarding").open){claimAutoResubmit=true;$("claim-form").requestSubmit();}}},1000);
    }
  } finally { button.disabled=false; }
});
$("pause-button").addEventListener("click",async()=>{try{const resume=status?.state==="paused";render(unwrap(await call("Pause",resume?0:3600))||{});toast(t(resume?"resumed":"paused_hour"));}catch(error){toast(error.message||error);}});
$("mode-local").addEventListener("click",async()=>{try{render(unwrap(await call("SetMode","standalone"))||{});toast(t("local_mode_on"));}catch(error){toast(error.message||error);}});
$("mode-connected").addEventListener("click",async()=>{try{render(unwrap(await call("SetMode","connected"))||{});toast(t("connected_mode_on"));}catch(error){openOnboarding();toast(error.message||error);}});
$("autostart").addEventListener("change",async(event)=>{const enabled=event.target.checked;event.target.disabled=true;try{await call("SetAutostart",enabled);toast(t(enabled?"autostart_on":"autostart_off"));}catch(error){event.target.checked=!enabled;toast(error.message||error);}finally{event.target.disabled=false;}});
$("ssid-consent").addEventListener("change",async(event)=>{const enabled=event.target.checked;event.target.disabled=true;try{render(unwrap(await call("SetSSIDConsent",enabled))||{});toast(t(enabled?"ssid_on":"ssid_off"));}catch(error){event.target.checked=!enabled;toast(error.message||error);}finally{event.target.disabled=false;}});
$("nerd-mode").addEventListener("change",(event)=>{nerdMode=event.target.checked;document.body.classList.toggle("nerd",nerdMode);try{localStorage.setItem("pulse:nerd-mode",nerdMode?"1":"0");}catch{}if(nerdMode)drawChart();if(!nerdMode&&$("screen-history").classList.contains("active")){document.querySelector('[data-screen="home"]').click();}});
$("disconnect-button").addEventListener("click",async()=>{if(!confirm(t("disconnect_confirm")))return;try{await call("Disconnect");status={state:"active",mode:"standalone",connected:false};samples=[];lastSpeedSummary="";lastSpeedResult=null;render();toast(t("disconnected_local"));}catch(error){toast(error.message||error);}});
$("speed-ring").addEventListener("click",async()=>{
  setSpeedControlsRunning(true);
  speedLive=null;$("speed-live").classList.add("hidden");
  resetSpeedDisplays();
  try {
    // An empty profile lets the service pick its default (household when offered).
    const profile=$("speed-profile").value||"";
    const result=unwrap(await call("RunTest",profile));
    renderSpeedResult(result);
    lastSpeedSummary=speedSummary(result);renderSchedule();
    // An eligible run is the freshest source of truth and is already rendered;
    // only fall back to history when this run was excluded from Home.
    if (result.experience_eligible === false) loadHomeSpeed(true);
    loadSpeedHistory();
    const errors=speedErrors(result);toast(errors.length?t("speed_incomplete",{reason:errors.join("; ")}):t("speed_complete"));
  } catch(error) { toast(error.message||error); }
  finally { setSpeedControlsRunning(false); }
});
$("household-fields").addEventListener("click",(event)=>{
  const button=event.target.closest("button[data-delta]");
  if(!button)return;
  const input=$("household-fields").querySelector(`input[data-field="${button.dataset.field}"]`);
  input.value=String(Math.min(4,Math.max(0,(Number(input.value)||0)+Number(button.dataset.delta))));
  householdEditor.dirty=true;renderHouseholdDemand();
});
$("household-editor").addEventListener("input",()=>{householdEditor.dirty=true;renderHouseholdDemand();});
async function applyHouseholdMix(mix,message){
  const save=$("household-save"),reset=$("household-reset");
  save.disabled=true;reset.disabled=true;
  try{
    const fresh=unwrap(await call("SetHouseholdMix",mix))||{};
    // The refreshed status is authoritative: let it repopulate the editor.
    householdEditor.dirty=false;householdEditor.applied="";
    render(fresh);
    toast(t(message));
  }catch(error){$("household-error").textContent=error.message||String(error);}
  finally{renderHouseholdEditor();renderHouseholdDemand();}
}
$("household-editor").addEventListener("submit",(event)=>{
  event.preventDefault();
  const mix=readEditorMix();
  if(!validateMix(mix).ok){renderHouseholdDemand();return;}
  applyHouseholdMix(normalizeMix(mix),"mix_saved");
});
$("household-reset").addEventListener("click",()=>applyHouseholdMix(null,"mix_cleared"));
$("support-button").addEventListener("click",async()=>{const button=$("support-button");button.disabled=true;try{const path=unwrap(await call("ExportSupportBundle"));if(path)toast(t("support_exported"));}catch(error){toast(error.message||error);}finally{button.disabled=false;}});
const updateControls = setupUpdates({
  button: $("update-button"), message: $("update-status"), stream: $("update-stream"),
  streamSetting: $("update-stream-setting"), managedHelp: $("apt-update-help"),
  call: async (...args) => unwrap(await call(...args)) || {}, t, toast,
  onChannelChange: (channel) => { status = {...(status || {}), update_channel: channel}; },
});

function applyClaim(claim){claim=unwrap(claim);if(!claim)return;$("claim-url").value=claim.url||"";$("claim-token").value=claim.token||"";openOnboarding();}
// Onboarding must be answered during first-run setup, but a claim-link dialog
// on an already-configured install stays dismissable with Escape.
$('onboarding').addEventListener('cancel',(event)=>{if(status?.state==="setup"){event.preventDefault();}else{clearClaimRetry();}});
$('onboarding-close').addEventListener('click',closeOnboarding);
window.addEventListener("resize",()=>{drawChart();drawSpeedLive();});document.addEventListener("visibilitychange",()=>{if(!document.hidden){drawChart();drawSpeedLive();}});
translate();
window.addEventListener("DOMContentLoaded",async()=>{
  // Subscribe before the first awaited IPC call so no status/claim event
  // emitted during startup is missed.
  window.wails.Events.On("pulse:status",(event)=>render(event.data||event));
  window.wails.Events.On("pulse:claim",(event)=>applyClaim(event.data||event));
  document.body.classList.toggle("nerd",nerdMode);$("nerd-mode").checked=nerdMode;
  try{render(unwrap(await call("Status"))||{});}catch(error){render({state:"service_unavailable"});}
  try{updateControls.setDesktopVersion(unwrap(await call("DesktopVersion")));}catch{}
  loadHomeSpeed();
  loadSpeedHistory();
  try{$("autostart").checked=Boolean(unwrap(await call("AutostartStatus")));}catch{$("autostart").disabled=true;}
  try{applyClaim(await call("PendingClaim"));}catch{}
});
