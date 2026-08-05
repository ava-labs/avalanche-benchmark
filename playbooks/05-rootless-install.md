# Playbook 05: rootless install

Goal: the whole fleet under a plain user account. No sudo anywhere, ever.

## Configure

```bash
# .env on the control machine
REMOTE_DIR=/home/youruser/avalanche     # everything lives under this
REMOTE_DATA_DIR=/nvme/youruser/data     # optional: databases on faster disk
```

Empty `REMOTE_DIR` means the system install (/opt, /etc, /var/lib, systemd,
sudo). Setting it switches every command to user mode: packages, configs and
data under your directory, nodes run as plain processes from a rendered
run.sh with a pidfile. `REMOTE_DATA_DIR` defaults to `REMOTE_DIR/data`.

`fleet deploy <mode> --dry-run` preflights every host without changing
anything; run it first on new machines. It checks exactly what user mode
needs: writable target paths, required tools, disk. Nothing assumes your
group name matches your username (RHEL-friendly) and staging paths are
per-user, so shared hosts do not collide.

## What is different from a system install

- No boot persistence and no auto-restart: after a host reboot, run
  `./bin/fleet pchain start` and then `./bin/fleet start`. If you want
  crash recovery, wrap those two in your own supervisor or cron.
- Everything else behaves identically: deploy, status, drills, place.

## Limits

One running install per set of machines: ports are assigned positionally
per host, so a second kit deployed beside a running one fails loudly at
start (it can no longer harm the first: process management is scoped to
each install's own directories). Validate a new release against the hosts
with `--dry-run` instead.
