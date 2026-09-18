// Keep long-running update requests visible and serialize update controls,
// including when live service status arrives while a request is in flight.
//
// The controls track two versions: the service's (from status) and this
// window's own build. They diverge when an update restarted the service but
// the hand-off to a fresh window did not happen; the button then offers a
// restart instead of pretending the update is fully loaded.
//
// APT-managed installs (status.update_method === "apt") hide the self-update
// controls entirely and show package instructions instead.
export function setupUpdates({button, message, stream, streamSetting, managedHelp, call, t, toast, onChannelChange, waitForRestart = waitForUpdateRestart, desktopVersion = ""}) {
  let status = {};
  let busy = false;
  let available = false;
  let restartNeeded = false;
  let progress = null;
  let guiVersion = normalizeVersion(desktopVersion);

  function mismatch(serviceVersion) {
    const service = normalizeVersion(serviceVersion);
    return Boolean(guiVersion && service && !guiVersion.includes("dev") && !service.includes("dev") && guiVersion !== service);
  }

  function progressLabel() {
    if (progress.done) return t("update_verifying");
    if (progress.total_bytes > 0) {
      const percent = Math.min(99, Math.floor((progress.downloaded_bytes * 100) / progress.total_bytes));
      return t("update_downloading_pct", {percent});
    }
    return t("update_downloading");
  }

  function renderControls() {
    const managed = status.update_method === "apt";
    if (streamSetting) streamSetting.classList[managed ? "add" : "remove"]("hidden");
    if (managedHelp) managedHelp.classList[managed ? "remove" : "add"]("hidden");
    // An APT-managed service that restarted into a newer package than this
    // window still offers Restart Pulse; everything else stays hidden.
    if (managed && !restartNeeded) {
      button.classList.add("hidden");
      message.textContent = t("update_apt_managed", {version: status.version || status.current_version || ""});
    }
    button.disabled = (managed && !restartNeeded) || busy || status.state === "service_unavailable";
    stream.disabled = managed || busy || Boolean(status.update_channel_locked) || status.state === "service_unavailable";
    button.setAttribute("aria-busy", String(busy));
    if (busy) {
      if (progress) button.textContent = progressLabel();
      return;
    }
    stream.value = status.update_channel || "main";
    button.textContent = t(restartNeeded ? "restart_pulse" : available ? "install_update" : "check_update");
  }

  function noteRestartNeeded(version) {
    restartNeeded = true;
    available = false;
    button.classList.remove("hidden");
    message.textContent = t("update_restart_needed", {version});
    renderControls();
  }

  function applyResult(result) {
    if (result.update_method === "apt") {
      status = {...status, ...result};
      available = false;
      renderControls();
      return;
    }
    available = Boolean(result.available_version);
    button.classList.remove("hidden");
    const streamName = t(`stream_${result.channel || status.update_channel || "main"}`);
    message.textContent = available
      ? t("update_available", {version: result.available_version})
      : result.check_error
        ? t("update_check_failed", {stream: streamName, error: result.check_error})
        : t("up_to_date", {version: result.current_version});
  }

  async function run(operation, label = "checking", progressText = "update_check_progress") {
    busy = true;
    progress = null;
    button.classList.remove("hidden");
    renderControls();
    button.textContent = t(label);
    message.textContent = t(progressText);
    try {
      await operation();
    } catch (error) {
      const detail = error?.message || String(error);
      message.textContent = t("update_failed", {error: detail});
      toast(detail);
    } finally {
      busy = false;
      progress = null;
      renderControls();
    }
  }

  button.addEventListener("click", async () => {
    if (busy || button.disabled) return;
    if (restartNeeded) {
      await run(async () => {
        await call("RelaunchDesktop");
        message.textContent = t("update_relaunching");
      }, "update_relaunching", "update_relaunching");
      return;
    }
    await run(async () => {
      if (available) {
        const result = await call("StageUpdate");
        progress = null;
        message.textContent = t(result.restarting ? "update_restarting" : "update_staged", {version: result.available_version});
        button.classList.add("hidden");
        toast(t("update_verified"));
        if (result.restarting) {
          button.classList.remove("hidden");
          button.textContent = t("update_restarting_service");
          const outcome = await waitForRestart(result.available_version, () => call("Status"));
          available = false;
          const version = outcome.status?.version;
          if (outcome.status) status = {...status, ...outcome.status};
          if (outcome.state === "ready" && mismatch(version)) {
            noteRestartNeeded(version);
            return;
          }
          message.textContent = t(outcome.state === "ready" ? "update_installed" : version ? "update_restart_failed" : "update_restart_unavailable", {version});
        }
      } else {
        applyResult(await call("CheckUpdate"));
      }
    }, available ? "update_downloading" : "checking", available ? "update_download_progress" : "update_check_progress");
  });

  // Stream selection also checks for updates, so it uses the same busy state.
  stream.addEventListener("change", async () => {
    if (busy || stream.disabled) return;
    const next = stream.value;
    await run(async () => {
      const result = await call("SetUpdateChannel", next);
      const channel = result.channel || next;
      status = {...status, update_channel: channel};
      onChannelChange(channel);
      applyResult(result);
      toast(t("stream_changed", {stream: t(`stream_${channel}`)}));
    });
  });

  return {
    renderStatus(next) {
      status = next;
      if (!busy && !restartNeeded && mismatch(next.version)) {
        noteRestartNeeded(next.version);
        return;
      }
      renderControls();
    },
    // Live download progress from the service while StageUpdate is in flight.
    renderProgress(next) {
      if (!busy || !next) return;
      progress = next;
      renderControls();
      message.textContent = progress.done
        ? t("update_verify_progress")
        : t("update_download_progress_bytes", {downloaded: megabytes(progress.downloaded_bytes), total: megabytes(progress.total_bytes)});
    },
    setDesktopVersion(version) {
      guiVersion = normalizeVersion(version);
      if (!busy && !restartNeeded && mismatch(status.version)) noteRestartNeeded(status.version);
    },
  };
}

function normalizeVersion(value) {
  return String(value || "").trim().replace(/^v/, "");
}

function megabytes(bytes) {
  return (Math.max(Number(bytes) || 0, 0) / 1_000_000).toFixed(1);
}

// Observe the local service, independently of release-index availability.
// A live monitoring event alone does not prove that the new version started.
export function waitForUpdateRestart(version, readStatus, {timeoutMS = 90000, intervalMS = 1000} = {}) {
  const normalize = normalizeVersion;
  return new Promise((resolve) => {
    let finished = false;
    let retry;
    let lastStatus = null;
    const finish = (state) => {
      if (finished) return;
      finished = true;
      clearTimeout(deadline);
      clearTimeout(retry);
      resolve({state, status: lastStatus});
    };
    // Keep a separate deadline: even an IPC call that never settles must not
    // leave the update controls hidden indefinitely.
    const deadline = setTimeout(() => finish("timeout"), timeoutMS);
    const poll = async () => {
      try {
        const status = await readStatus();
        if (finished) return;
        lastStatus = status;
        if (normalize(version) && normalize(status?.version) === normalize(version)) {
          finish("ready");
          return;
        }
      } catch {
        if (finished) return;
        lastStatus = null;
      }
      retry = setTimeout(poll, intervalMS);
    };
    poll();
  });
}
