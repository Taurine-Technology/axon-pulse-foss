# ISP operations

## Subscriber invite playbook

1. In Axon, open **Network → Sensors → Invite**.
2. Select the subscriber's site, choose single-use unless this is a managed
   fleet deployment, and keep the default measurement profile unless the
   subscriber has an unusually small data cap.
3. Send the generated claim link. Do not send screenshots of or repeatedly
   paste the raw token; the link expires and enrollment consumes a single-use
   token atomically.
4. Ask the subscriber to download Pulse and click **Connect Pulse** on the same
   page. The default consumer installer is per-user and never prompts for an
   administrator password or UAC.
5. Confirm the sensor becomes **Active** within two minutes. The detail page
   separates gateway quality from internet quality and shows clock/data gaps.

Pause a sensor before a planned subscriber maintenance window. Revoke it when
the device is lost or retired; its next request wipes local credentials and
queued data. Revoking an invite prevents new claims but does not revoke sensors
already claimed from it.

## Subscriber FAQ template

**Can Pulse see what I browse?** No. It measures only its own small synthetic
requests and sends aggregates, never packet contents or browsing history.

**How much data does it use?** Basic checks are roughly 1–3 MB/day. Scheduled
speed tests size themselves to the link: they spend only what measuring your
speed actually needs, so faster connections use proportionally more data.
Scheduled tests obey the displayed daily and monthly budgets (1 GB/day and
20 GB/month by default) and defer on a metered link, low battery (below 20%),
or while the connection is busy. A test you start yourself always runs — it
notes those conditions on the result, and warns (without blocking) when it
will exceed the remaining budget.

**Does it install a VPN?** No. Pulse never joins the provider's private network
and receives no broker or switch credentials.

**Can I stop it?** Pause from the tray or Settings. Disconnect or uninstall to
remove credentials and local history.

**Can I try new versions early?** Yes. Open Settings and change **Update
stream** from Stable to Beta or Alpha; Pulse checks that stream straight away
and switches back just as easily. Managed installations may have the stream
fixed by the administrator.

## Managed installations

The managed Linux unit runs before login. Managed macOS launchd and Windows
service installers require a one-time administrator action; consumer packages
remain per-user/login-persistent. Both modes use identical measurement,
security, and API code.

## Soak and recovery gate

Run the owned performance harness on Linux or macOS (a real claim exercises the
full probe/uploader path):

```sh
go run ./tools/soak --binary ./bin/axon-pulse --duration 24h \
  --url https://controller.example --token spt_...
```

It fails if average process CPU reaches 1% or RSS exceeds 40 MiB, and includes
the final queue/upload/data-budget status in its JSON evidence. During the run,
disconnect the network for an hour and send `kill -9` once; after restart,
`axon-pulse status` plus SQLite `PRAGMA quick_check` must show an intact queue
that drains with original timestamps. Use `scripts/pulse-loadtest` in the
controller repo for the 2,000/5,000-client ingest gate.
