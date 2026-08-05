# Playbook 05: rootless install

This playbook runs the full fleet under a normal user account. No command
uses sudo.

## Configuration

Set these values in `.env` on the control machine:

```bash
REMOTE_DIR=/home/youruser/avalanche     # all fleet files go under this directory
REMOTE_DATA_DIR=/nvme/youruser/data     # optional: databases on a faster disk
```

An empty `REMOTE_DIR` selects the system install. The system install uses
/opt, /etc, /var/lib, systemd, and sudo. A set `REMOTE_DIR` selects the
user install. In the user install, all files are under your directory.
Each node runs as a normal process. A run script starts it, and a pidfile
identifies it. `REMOTE_DATA_DIR` is optional. Its default is
`REMOTE_DIR/data`.

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

## Differences from the system install

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
