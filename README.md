# Universal-server-manager

Install on the jump server:

```sh
curl -fsSL https://raw.githubusercontent.com/teinfai/Universal-server-manager/main/install.sh | sudo bash
```

Uninstall:

```sh
curl -fsSL https://raw.githubusercontent.com/teinfai/Universal-server-manager/main/install.sh | sudo bash -s -- --uninstall
```

USM runs on a jump server and updates one GitLab server at 02:00 each night.
It follows GitLab's required upgrade stops, installs one version, and reports
each action to Telegram. Before installing, it backs up GitLab and its settings,
pauses runners, and waits for running CI jobs to finish. It checks GitLab after
installing, resumes the runners, and removes older USM backups.

A failed action stops future updates until you run `sudo -u usm usm resume`.
USM never rolls back automatically or kills CI jobs. `sudo -u usm usm run --now`
bypasses the latest start time and the one-install-per-day guard.

## Requirements

- A GitLab Linux package on Ubuntu or Debian, with its apt repository set up.
- A Linux jump server with systemd, SSH access to GitLab, access to GitLab's
  API, and outbound HTTPS to `gitlab.com` and `api.telegram.org`.
- Go 1.27 to build the program. The GitLab server also needs systemd for
  update steps that keep running if SSH disconnects.

## Manual install

### Build

For an amd64 jump server:

```sh
GOOS=linux GOARCH=amd64 go build -o usm ./cmd/usm
```

For an arm64 jump server:

```sh
GOOS=linux GOARCH=arm64 go build -o usm ./cmd/usm
```

### Jump server setup

Create the user and folders, copy the binary, and install the example config:

```sh
sudo useradd --system --home-dir /var/lib/usm --shell /usr/sbin/nologin usm
sudo install -d -o usm -g usm -m 0700 /etc/usm
sudo install -d -o usm -g usm -m 0750 /var/lib/usm /var/log/usm
sudo install -m 0755 usm /usr/local/bin/usm
sudo install -o usm -g usm -m 0600 deploy/config.example.yaml /etc/usm/config.yaml
sudo -u usm ssh-keygen -t ed25519 -N "" -f /etc/usm/id_ed25519
```

Edit `/etc/usm/config.yaml` with the real server address, SSH settings, GitLab
address and tokens. Every field is required. Keep the config owned by `usm`
with mode 0600. USM refuses a config that allows group or other access.

Set `gitlab.backup_dir` to GitLab's actual backup path. The default is
`/var/opt/gitlab/backups`; GitLab's `gitlab_rails['backup_path']` setting can
change it. USM does not change where GitLab writes its backup. If these paths
differ, the Backup action cannot find the file and fails before updating GitLab.

Keep `schedule.timezone` and the time zone in `deploy/usm.timer` the same.
The supplied files both use `Asia/Kuala_Lumpur`. `schedule.latest_start` is
the latest permitted start time, and `schedule.ci_wait_minutes` controls how
long USM waits for CI jobs.

After completing the GitLab server and token setup below, check the connection
with `sudo -u usm usm test`. Copy the units and enable the timer:

```sh
sudo cp deploy/usm.service deploy/usm.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now usm.timer
```

### GitLab server setup

On the GitLab server, create an account for the jump server:

```sh
sudo useradd --create-home --shell /bin/bash usm
sudo install -d -o usm -g usm -m 0700 /home/usm/.ssh
```

Copy the jump server's `/etc/usm/id_ed25519.pub` into
`/home/usm/.ssh/authorized_keys` on GitLab. Set its owner and permissions:

```sh
sudo chown usm:usm /home/usm/.ssh/authorized_keys
sudo chmod 0600 /home/usm/.ssh/authorized_keys
```

Create `/etc/sudoers.d/usm` containing this line and give it mode 0440:

```text
usm ALL=(ALL) NOPASSWD:ALL
```

On the jump server, as root, replace `<host>` with the GitLab server address
and scan its SSH host key. Use the configured port if it differs from 22:

```sh
ssh-keyscan -p 22 <host> > /etc/usm/known_hosts
ssh-keygen -lf /etc/usm/known_hosts
chown usm:usm /etc/usm/known_hosts
```

Verify the displayed fingerprint against the GitLab server's host key before
using it. USM uses this pinned file and refuses a different host key.

### GitLab and Telegram tokens

Use a GitLab administrator token with the `api` scope for `gitlab.token`.
USM needs to read versions and runners, pause runners, and resume them.

Create a Telegram bot, add it to the group that should receive updates, and
put its token and the group's chat ID in `telegram.bot_token` and
`telegram.chat_id`. `sudo -u usm usm test` sends `usm test message` to that group.

## Commands

Run every USM command as the `usm` user: `sudo -u usm usm <command>`.
Every command accepts `--config <path>`. The default is `/etc/usm/config.yaml`.

| Command | What it does |
|---|---|
| `sudo -u usm usm run` | Runs the nightly checks and installs at most one version. Skips if it is too late or a version was already attempted today. |
| `sudo -u usm usm run --now` | Runs the same flow without the time and date guards. It can install another version on the same day. |
| `sudo -u usm usm status` | Prints the saved state, then checks SSH, sudo, the installed package, available packages and the upgrade path. It changes no state and sends no Telegram message. |
| `sudo -u usm usm resume` | Resumes recorded paused runners, then clears the stop flag if all resumed. Refuses while another run holds the lock. |
| `sudo -u usm usm restore` | Shows the recorded backup and version, without restoring. |
| `sudo -u usm usm restore --yes` | Restores that backup and version, checks GitLab, and resumes runners. Leaves the stop flag unchanged. |
| `sudo -u usm usm test` | Checks SSH, sudo, the package, services, API version, upgrade stops, runner access and Telegram. Writes no state. |

State is in `/var/lib/usm/state.json` by default. The daily append-only log is
`/var/log/usm/YYYY-MM-DD.log`, dated in the configured time zone. Telegram gets
short action messages; command output and errors go to the log.

## Backup limits

The backup does not include object storage, the container registry or an
outside database. Arrange separate backups for these before relying on a
restore. Keep GitLab's default database backup during package upgrades enabled.

## Restore by hand

Run `sudo -u usm usm restore` to inspect the saved backup. To let USM perform
the restore, run `sudo -u usm usm restore --yes`. After checking GitLab, run
`sudo -u usm usm resume` to allow future updates. If a remote step is still
running, wait for it to finish first.

For a manual restore, read `last_backup` in the jump server's state file. Use
its `package`, full `version`, `name` and `etc_tar` for the placeholders below.
Pause runners and wait for running jobs to finish before changing GitLab.
Keep the backup in GitLab's configured backup directory.

Run these commands as root on the GitLab server, replacing each placeholder
with the recorded value:

```sh
gitlab-ctl stop puma
gitlab-ctl stop sidekiq
apt-mark unhold <package> || true
dpkg -r <package>
DEBIAN_FRONTEND=noninteractive apt-get install -y <package>=<full-version>
apt-mark hold <package>
mv /etc/gitlab /etc/gitlab.usm-$(date +%s)
tar -xf <etc-tar> -C /
gitlab-ctl reconfigure
gitlab-ctl stop puma
gitlab-ctl stop sidekiq
GITLAB_ASSUME_YES=1 gitlab-backup restore BACKUP=<backup-name>
gitlab-ctl reconfigure
gitlab-ctl restart
```

Check that the installed package and GitLab API report the recorded version
and that GitLab's services are running. Resume the runners you paused, then
run `sudo -u usm usm resume` on the jump server to clear USM's stop flag.
