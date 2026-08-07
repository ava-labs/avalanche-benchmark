# Playbook 02: rootless install

The fleet runs under a normal user account by default. No command uses
sudo. This playbook describes the defaults, the overrides, and the legacy
root install.

## Configuration

The default install root is `/home/<SSH_USER>/avalanche-benchmark`. You
configure nothing for it. In the user install, all files are under that
directory. Each node runs as a normal process. A run script starts it, and
a pidfile identifies it.

Two optional overrides exist in `.env` on the control machine:

```bash
REMOTE_DIR=/home/youruser/avalanche     # a different install root
REMOTE_DATA_DIR=/nvme/youruser/data     # databases and logs on a faster disk
```

`REMOTE_DATA_DIR` defaults to the `data/` subdirectory of the install
root. Set `REMOTE_DIR` when the ssh user's home is not under `/home`, for
example for root.

`SYSTEM_INSTALL=true` selects the legacy root install instead: /opt, /etc,
/var/lib, systemd units, and sudo everywhere. It gives restart-on-crash
and start-on-boot, and it needs passwordless sudo on every machine. It
cannot be combined with `REMOTE_DIR` or `REMOTE_DATA_DIR`.

## Test the hosts first

```bash
./bin/fleet deploy frozen --dry-run
```

This command examines every host and changes nothing. It checks the ssh
access, the necessary tools, the write permissions on the target paths,
and the free disk space. Run it on every new set of machines before the
first deploy.

The user install does not use Linux group names. It works on hosts where
your group name is not your user name, for example on RHEL. The staging
paths in /tmp include your user name, so two operators on one host do not
collide.

## Differences from the legacy root install

- No node starts at boot. No node restarts after a crash. After a host
  reboot, run `./bin/fleet pchain start`, then `./bin/fleet start`. If
  you want automatic recovery, put these two commands in your own
  supervisor or cron.
- All other commands are identical: deploy, status, the drills, place.

## Known limit

Only one install can run on a set of machines at one time. The node ports
are fixed per host. A second install beside a running one stops with a
port error at start. The second install cannot damage the first one:
process control is limited to each install's own directories. To validate
a new release against the hosts, use `--dry-run`.
