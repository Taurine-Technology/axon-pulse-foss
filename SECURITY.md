# Security policy

Axon Pulse is a credential-bearing, self-updating agent that runs on
subscriber machines. Please report vulnerabilities privately; do not open a
public issue or pull request for a security problem.

## Reporting a vulnerability

Preferred: use GitHub's private vulnerability reporting for this repository
(**Security → Report a vulnerability**), which creates a private advisory
visible only to the maintainers.

Alternative: email `info@taurinetech.com` with the subject line
`Pulse security report`. Do not include secrets, claim tokens, or personal
data from other people in the report.

A useful report contains the Pulse version (`axon-pulse version` or
**Settings → About**), the operating system and install method (desktop,
headless, APT, AppImage), the mode (local or connected), reproduction steps,
and the impact you believe the issue has.

## What to expect

- Acknowledgement within three business days.
- An initial assessment, including severity and whether a fix is planned,
  within ten business days.
- Fixes for confirmed issues ship through the normal update streams. Critical
  issues are released as soon as a fix is verified; other issues ship in the
  next scheduled release.
- We coordinate disclosure with the reporter and aim to publish within 90 days
  of the report, or sooner once a fix is available to users.
- Reporters are credited in the release notes unless they prefer otherwise.

## Scope

In scope: everything built from this repository, including the measurement
service, the desktop application, the CLI, the local IPC socket or named pipe,
the update download and verification path, the installers, and the packaging
scripts.

Out of scope: the hosted Axon controller and console (report those to Taurine
Technology directly), third-party public endpoints used for measurements,
denial of service against those endpoints, and social-engineering attacks on
users or maintainers.

## Supported versions

Security fixes are applied to the latest release on the `main` update stream.
Installations on the `beta` and `alpha` streams receive the fix in the next
build of that stream. Older releases are not patched; update to the latest
release.

## Good-faith research

Research that respects users' privacy, avoids data destruction, stays within
systems you own or are authorised to test, and reports findings promptly is
welcome. We will not pursue action against researchers who follow this
policy.
