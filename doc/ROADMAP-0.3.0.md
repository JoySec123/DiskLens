# DiskLens 0.3.0: space ledger and scheduled inspection

## Scope

Continue the native fnOS, administrator-only, read-only application. Add a
space-change ledger, low-impact scheduled scans and persistent in-app alerts.
Do not add deletion, migrations, system firewall changes or automatic cleanup.
Network management belongs to the separate NetLens project.

## Acceptance Requirements

- [ ] One persisted daily schedule with explicit timezone, scan roots and exclusions.
  Disabled by default. Run at most once per local calendar day, within a one-hour
  start window; defer while another disk task is running and do not replay missed
  windows after downtime. Revalidate directory authorization before every run.
- [ ] Configurable metadata scan rate and maximum duration (up to 24 hours),
  applied to manual and scheduled scans. Cancellation and shutdown remain bounded
  by any active filesystem call. Never promise that scans will not wake disks.
- [ ] Separate intentionally excluded entries, symbolic links and read errors.
  Incomplete coverage must not generate an authoritative growth alert.
- [ ] Persist successful scan summaries for at most 365 days / 2,000 records,
  independently of the existing 20 full file indexes. Store all first-level
  directory totals and root-level file totals, not only a Top-N list.
- [ ] Compare only identical canonical roots and exclusion rules. Show total,
  directory and category changes, exact baseline timestamps and coverage status.
  Failed/cancelled scans never publish a summary. Old full indexes need not exist
  for summary comparison. Summaries are not filesystem snapshots or live state.
- [ ] Record filesystem capacity at scan completion separately from logical file
  sizes. Do not infer physical reclaimable bytes or forecast disk exhaustion from
  logical sizes. Mark unavailable capacity measurements explicitly.
- [ ] Persistent growth and low-space alerts, configurable thresholds and read
  acknowledgement. Alerts are in-app only in this version, with no implicit
  external messages. Every result and alert observes current directory access.
- [ ] Upgrade from 0.2.0 without altering source files or losing existing history.
  Test permission revocation, scheduling, cancellation, persistence and retention.
- [ ] White workspace / left navigation, desktop and narrow-screen browser tests,
  actual UI screenshots, race tests, Linux amd64/arm64 builds and FPK validation.

## Delivery Gates

The user supplied an ARM64 fnOS test VM on 2026-10-19. Version 0.3.0 was installed
and started there; its desktop entry is visible. Gateway authentication, ACL
changes, startup/shutdown and upgrades still require separate observed results.
The supplied fnOS skill has been installed and its OAuth login is awaiting user
authorization. Never store device passwords or OAuth secrets in project files.
The duplicate embedded title correction was packaged in 0.3.1 and is also included
in 0.3.2. The VM reproduced a gateway Host-port normalization bug in 0.3.0;
0.3.2 corrects mutation validation and passes a local Unix-socket proxy browser
regression, but has not yet been upgraded on the VM. See VERIFICATION-0.3.2.md.
Version 0.3.3 adds startup/idle retention maintenance and immediate filtering of
expired ledger records, locally tested and packaged. See VERIFICATION-0.3.3.md.

## Next Product

After this DiskLens increment, create a separate NetLens project and develop it
with explicit capability reporting, application/container attribution,
connection audit, traffic history and scoped controls. Market differentiation is
a hypothesis, not a claim of worldwide novelty. Keep its permission boundary
separate from DiskLens. A simulated dashboard alone is not a finished product.
