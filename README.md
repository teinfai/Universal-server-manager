# Universal Server Manager (USM)

USM keeps your GitLab up to date by itself. Every night, at a time you pick, it backs up
GitLab, installs the next version, checks that GitLab still works, and sends
each step to your Telegram group.

It works with GitLab installed with apt on Ubuntu or Debian (amd64 or arm64).

## Before you start

Get these two things ready:

1. **Telegram bot token.** In Telegram, open
   [@BotFather](https://t.me/BotFather), send `/newbot`, and follow the
   steps. Copy the token it gives you.
2. **Telegram group ID.** Add your bot to your group. In the group, send
   `/start@your_bot_username`. Then open this link in a browser, with your
   bot token in place of `<bot-token>`:

   ```text
   https://api.telegram.org/bot<bot-token>/getUpdates
   ```

   Find `"chat":{"id":` and copy the number after it, including the `-`.

## Install

Run this on your GitLab server:

```sh
curl -fsSL https://teinfai.github.io/usm.sh | sudo bash
```

Most questions show a default in `[brackets]`. Press Enter to use it. You
only need to type:

- your GitLab web address, like `https://gitlab.example.com`
- the Telegram bot token
- the Telegram group ID
- the update time in 24-hour format, like `23:30` (or press Enter for `02:00`)

At the end, USM sends a test message to your Telegram group. If you see it,
you are done.

**USM on a different server?** Answer `n` to `Is GitLab on this server?`.
The installer then shows a few commands to run on the GitLab server. SSH must
be on, on the GitLab server. USM uses SSH even when it runs on the same
server.

## Update USM

```sh
sudo usm update
```

It installs the newest USM, keeps your settings, and runs all the
checks again. GitLab and your backups are not touched. USM older than
v0.5.0 doesn't have this command; run the install line once instead.

## What it does every night

1. Deletes the previous USM backup, then backs up GitLab and its settings.
2. Installs the next version on GitLab's official upgrade path. One
   version per night.
3. Checks that GitLab works.
4. Keeps this backup until the next update, so you can go back later. If
   the update fails, USM puts the backup back by itself, then stops and
   tells your Telegram group.

USM does not pause CI. GitLab restarts during the update, so pick a
time when nobody runs CI jobs.

## If something goes wrong

- If a step fails, USM stops and tells your Telegram group. Fix the problem,
  then run `sudo -u usm usm resume`.
- If an update fails, USM goes back to the backup by itself and stops.
  Check GitLab, then run `sudo -u usm usm resume`. If going back
  also fails, the backup stays, and you can try again with
  `sudo -u usm usm restore --yes`.
- Logs are in `/var/log/usm`.

## Commands

| Command | What it does |
|---|---|
| `sudo -u usm usm status` | Shows the installed version and the upgrade path. |
| `sudo -u usm usm test` | Checks every connection and sends a test message. |
| `sudo -u usm usm run --now` | Updates one version now, without waiting for the update time. |
| `sudo -u usm usm resume` | Starts the nightly updates again after a stop. |
| `sudo -u usm usm restore --yes` | Puts back the last backup (from before the last update). Changes made since then are lost. |
| `sudo usm update` | Installs the newest USM and keeps your settings. |
| `sudo usm uninstall` | Removes USM. Asks before deleting settings and logs. |

## Good to know

- Before each update, USM checks there is room for one full backup (it
  may delete its previous backup to make room). If not, it skips that
  night and tells your Telegram group how much space it needs.
  `usm test` checks this too.
- If an update fails, USM restores the backup by itself. It never touches
  GitLab after a good update.
- The backup does not include object storage or an outside database. Back
  those up yourself.
- GitLab's own small database backup before each update stays on. Delete
  old ones in the backup folder when you no longer need them.
- USM checks GitLab with GitLab's own readiness check, run on the GitLab
  server. No GitLab token is needed.

## Uninstall

```sh
sudo usm uninstall
```

USM older than v0.5.0: `curl -fsSL https://teinfai.github.io/usm.sh | sudo bash -s -- --uninstall`

## License

USM is free and open source under the [MIT License](LICENSE).
Copyright (c) 2026 teinfai.

USM comes with no warranty. It changes a live GitLab server, so try it on a
test copy first and keep your own backups.
