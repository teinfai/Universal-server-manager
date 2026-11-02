#!/usr/bin/env bash
set -euo pipefail

umask 077

if (( EUID != 0 )); then
    printf 'Run with sudo.\n'
    exit 1
fi

missing=()
for dependency in curl sha256sum ssh-keygen ssh-keyscan systemctl useradd timedatectl; do
    if ! command -v "$dependency" >/dev/null 2>&1; then
        missing+=("$dependency")
    fi
done
if (( ${#missing[@]} > 0 )); then
    printf 'Missing: %s\n' "${missing[*]}"
    exit 1
fi

cpu=$(uname -m)
case "$cpu" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) printf 'Unsupported CPU: %s\n' "$cpu"; exit 1 ;;
esac

timezone_pattern='^[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*$'

valid_timezone() {
    local value=$1
    [[ $value =~ $timezone_pattern ]] || return 1
    case "/$value/" in
        */./*|*/../*) return 1 ;;
    esac
    [[ -f /usr/share/zoneinfo/$value ]]
}

ask() {
    local question=$1 default=$2 pattern=$3 kind=${4:-text}
    while true; do
        printf '%s ' "$question" > /dev/tty
        if [[ $kind == secret ]]; then
            IFS= read -rs answer < /dev/tty
            printf '\n' > /dev/tty
        else
            IFS= read -r answer < /dev/tty
        fi
        answer=${answer:-$default}
        if [[ $answer =~ $pattern ]]; then
            case "$kind" in
                port)
                    if (( 10#$answer >= 1 && 10#$answer <= 65535 )); then
                        answer=$((10#$answer))
                        return
                    fi
                    ;;
                timezone)
                    if valid_timezone "$answer"; then
                        return
                    fi
                    ;;
                *) return ;;
            esac
        fi
        printf 'Invalid answer. Try again.\n' > /dev/tty
    done
}

ask_timezone() {
    local default_timezone
    default_timezone=$(timedatectl show -p Timezone --value)
    valid_timezone "$default_timezone" || default_timezone=UTC
    if ! valid_timezone "$default_timezone"; then
        printf 'Invalid system time zone.\n'
        exit 1
    fi
    ask "Time zone [$default_timezone]" "$default_timezone" "$timezone_pattern" timezone
    timezone=$answer
}

if [[ ${1:-} == --uninstall ]]; then
    systemctl disable --now usm.timer || true
    rm -f /etc/systemd/system/usm.service /etc/systemd/system/usm.timer /usr/local/bin/usm
    systemctl daemon-reload
    rm -f /etc/sudoers.d/usm /usr/local/sbin/usm-step.sh
    ask 'Also delete settings, state and logs? [y/N]' N '^[YyNn]$'
    if [[ $answer == [Yy] ]]; then
        rm -rf /etc/usm /var/lib/usm /var/log/usm
        userdel usm || true
    fi
    printf '%s\n' 'If GitLab is on another server, run these there:' \
        'userdel -r usm' \
        'rm -f /etc/sudoers.d/usm /usr/local/sbin/usm-step.sh'
    exit 0
fi

download_dir=$(mktemp -d /tmp/usm.XXXXXXXXXX)
if [[ ! $download_dir =~ ^/tmp/usm\.[A-Za-z0-9]{10}$ ]]; then
    printf 'Unexpected temporary folder.\n'
    exit 1
fi
trap 'rm -rf -- "$download_dir"' EXIT

release_url=https://github.com/teinfai/Universal-server-manager/releases/latest/download
curl -fsSL "$release_url/usm-linux-$arch" -o "$download_dir/usm-linux-$arch"
curl -fsSL "$release_url/SHA256SUMS" -o "$download_dir/SHA256SUMS"
if ! (
    cd "$download_dir" || exit 1
    grep " usm-linux-$arch\$" SHA256SUMS | sha256sum -c -
); then
    printf 'Checksum failed.\n'
    exit 1
fi
install -m 0755 "$download_dir/usm-linux-$arch" /usr/local/bin/usm

if ! id usm >/dev/null 2>&1; then
    useradd --system --home-dir /var/lib/usm --shell /usr/sbin/nologin usm
fi
install -d -o usm -g usm -m 0700 /etc/usm
install -d -o usm -g usm -m 0750 /var/lib/usm /var/log/usm

keep_settings=false
if [[ -e /etc/usm/config.yaml ]]; then
    ask 'Keep current settings? [Y/n]' Y '^[YyNn]$'
    if [[ $answer == [Yy] ]]; then
        keep_settings=true
        start_time=
        timezone=
        start_line_pattern='^  start: "(([01][0-9]|2[0-3]):[0-5][0-9])"$'
        timezone_line_pattern='^  timezone: "([^"]+)"$'
        while IFS= read -r line || [[ -n $line ]]; do
            if [[ $line =~ $start_line_pattern ]]; then
                start_time=${BASH_REMATCH[1]}
            elif [[ $line =~ $timezone_line_pattern ]]; then
                timezone=${BASH_REMATCH[1]}
            fi
        done < /etc/usm/config.yaml
        if [[ -z $start_time ]] || ! valid_timezone "$timezone"; then
            printf 'Settings are from an older version. Please answer the questions again.\n'
            keep_settings=false
        fi
    fi
fi

if [[ $keep_settings == false ]]; then
    ask 'Name for this GitLab server [gitlab]' gitlab '^[A-Za-z0-9._-]{1,40}$'
    server_name=$answer
    ask 'Is GitLab on this server? [Y/n]' Y '^[YyNn]$'
    same_server=$answer
    if [[ $same_server == [Yy] ]]; then
        ssh_host=127.0.0.1
    else
        ask 'GitLab server address (IP or host name)' '' '^[A-Za-z0-9.-]+$'
        ssh_host=$answer
    fi
    ask 'SSH port [22]' 22 '^[0-9]{1,5}$' port
    ssh_port=$answer
    ask 'GitLab web address (like https://gitlab.example.com)' '' '^https?://[A-Za-z0-9.:-]+/?$'
    gitlab_url=${answer%/}
    ask 'GitLab token (input hidden)' '' '^[A-Za-z0-9._-]{20,}$' secret
    gitlab_token=$answer
    ask 'Telegram bot token (input hidden)' '' '^[0-9]+:[A-Za-z0-9_-]+$' secret
    telegram_bot_token=$answer
    ask 'Telegram group ID (like -1001234567890)' '' '^-?[0-9]+$'
    telegram_chat_id=$answer
    ask_timezone
    ask 'Update time, 24-hour (like 02:00) [02:00]' 02:00 '^([01][0-9]|2[0-3]):[0-5][0-9]$'
    start_time=$answer
    ask 'Backup folder on the GitLab server [/var/opt/gitlab/backups]' /var/opt/gitlab/backups '^/[A-Za-z0-9._/-]+$'
    backup_dir=$answer

    if [[ ! -e /etc/usm/id_ed25519 ]]; then
        local_hostname=$(hostname)
        if [[ ! $local_hostname =~ ^[A-Za-z0-9.-]+$ ]]; then
            printf 'Unexpected host name format.\n'
            exit 1
        fi
        ssh-keygen -t ed25519 -N "" -C "usm@$local_hostname" -f /etc/usm/id_ed25519
    fi
    chown usm:usm /etc/usm/id_ed25519 /etc/usm/id_ed25519.pub
    mapfile -t public_key_lines < /etc/usm/id_ed25519.pub
    public_key_pattern='^ssh-ed25519 [A-Za-z0-9+/=]+ usm@[A-Za-z0-9.-]+$'
    if (( ${#public_key_lines[@]} != 1 )) || [[ ! ${public_key_lines[0]} =~ $public_key_pattern ]]; then
        printf 'Unexpected public key format.\n'
        exit 1
    fi
    public_key=${public_key_lines[0]}
    if [[ $same_server == [Yy] ]]; then
        usm_home=$(getent passwd usm | cut -d: -f6)
        if [[ ! $usm_home =~ ^/[A-Za-z0-9._/-]+$ ]]; then
            printf 'Unexpected home folder for usm.\n'
            exit 1
        fi
        usermod --shell /bin/bash usm
        install -d -o usm -g usm -m 0700 "$usm_home/.ssh"
        install -o usm -g usm -m 0600 /etc/usm/id_ed25519.pub "$usm_home/.ssh/authorized_keys"
        printf '%s\n' 'usm ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/usm
        chmod 0440 /etc/sudoers.d/usm
    else
        cat <<EOF
Run these on the GitLab server:
sudo useradd --create-home --shell /bin/bash usm
sudo install -d -o usm -g usm -m 0700 /home/usm/.ssh
echo '$public_key' | sudo tee /home/usm/.ssh/authorized_keys
sudo chown usm:usm /home/usm/.ssh/authorized_keys
sudo chmod 0600 /home/usm/.ssh/authorized_keys
echo 'usm ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/usm
sudo chmod 0440 /etc/sudoers.d/usm
EOF
        ask 'Press Enter when done' '' '^$'
    fi

    if ! ssh-keyscan -p "$ssh_port" -- "$ssh_host" > /etc/usm/known_hosts || [[ ! -s /etc/usm/known_hosts ]]; then
        printf 'Cannot reach %s:%s.\n' "$ssh_host" "$ssh_port"
        exit 1
    fi
    if [[ $same_server != [Yy] ]]; then
        ssh-keygen -lf /etc/usm/known_hosts
        ask "Is this the GitLab server's key? [y/N]" N '^[YyNn]$'
        if [[ $answer != [Yy] ]]; then
            printf 'Stopped. The key was not confirmed.\n'
            exit 1
        fi
    fi
    chown usm:usm /etc/usm/known_hosts

    install -o usm -g usm -m 0600 /dev/null /etc/usm/config.yaml
    cat > /etc/usm/config.yaml <<EOF
server_name: "$server_name"
ssh:
  host: "$ssh_host"
  port: $ssh_port
  user: "usm"
  key_file: "/etc/usm/id_ed25519"
  known_hosts_file: "/etc/usm/known_hosts"
gitlab:
  url: "$gitlab_url"
  token: "$gitlab_token"
  backup_dir: "$backup_dir"
telegram:
  bot_token: "$telegram_bot_token"
  chat_id: "$telegram_chat_id"
schedule:
  timezone: "$timezone"
  start: "$start_time"
state_dir: "/var/lib/usm"
log_dir: "/var/log/usm"
EOF
fi

cat > /etc/systemd/system/usm.service <<'EOF'
[Unit]
Description=Nightly GitLab upgrade
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=usm
ExecStart=/usr/local/bin/usm run
TimeoutStartSec=20h
EOF
chmod 0644 /etc/systemd/system/usm.service

cat > /etc/systemd/system/usm.timer <<EOF
[Unit]
Description=Run USM every night

[Timer]
OnCalendar=*-*-* $start_time:00 $timezone
Persistent=false

[Install]
WantedBy=timers.target
EOF
chmod 0644 /etc/systemd/system/usm.timer
systemctl daemon-reload

if ! runuser -u usm -- /usr/local/bin/usm test; then
    printf 'Fix the items above, then run the install line again.\n'
    exit 1
fi

systemctl enable usm.timer
systemctl restart usm.timer
printf 'Installed. GitLab updates every night at %s (%s).\n' "$start_time" "$timezone"
printf 'Check: sudo -u usm usm status\n'
exit 0
