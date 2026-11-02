#!/usr/bin/env bash
set -euo pipefail

[[ $# -ge 2 ]] || exit 64
id=$1
action=$2
shift 2
[[ $id =~ ^(backup|install|restore)-[0-9]{14}-[0-9a-f]{8}$ ]] || exit 64
[[ $action =~ ^(backup|install|restore)$ ]] || exit 64

case "$action" in
  backup)
    [[ $# -eq 2 ]] || exit 64
    name=$1
    dir=$2
    [[ $name =~ ^usm-[0-9]{8}-[0-9]{6}-[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 64
    [[ $dir =~ ^/[A-Za-z0-9._/-]+$ ]] || exit 64
    ;;
  install|restore)
    if [[ $action == install ]]; then
      [[ $# -eq 2 ]] || exit 64
    else
      [[ $# -eq 5 ]] || exit 64
    fi
    pkg=$1
    ver=$2
    [[ $pkg =~ ^gitlab-(ce|ee)$ ]] || exit 64
    [[ $ver =~ ^[0-9]+\.[0-9]+\.[0-9]+-(ce|ee)\.[0-9]+$ ]] || exit 64
    if [[ $action == restore ]]; then
      name=$3
      dir=$4
      etc_tar=$5
      [[ $name =~ ^usm-[0-9]{8}-[0-9]{6}-[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 64
      [[ $dir =~ ^/[A-Za-z0-9._/-]+$ ]] || exit 64
      [[ $etc_tar =~ ^/[A-Za-z0-9._/-]+/gitlab_config_[0-9_]+\.tar$ ]] || exit 64
    fi
    ;;
esac

mkdir -p /var/log/usm /var/lib/usm
exec >>"/var/log/usm/$id.log" 2>&1
trap 'printf "%s\n" "$?" >"/var/lib/usm/$id.status"' EXIT

case "$action" in
  backup)
    gitlab-backup create SKIP=tar
    mkdir -p "$dir/$name-etc"
    gitlab-ctl backup-etc --backup-path "$dir/$name-etc"
    ;;
  install)
    apt-mark unhold "$pkg" || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$pkg=$ver"
    apt-mark hold "$pkg"
    ;;
  restore)
    [[ -s "$dir/backup_information.yml" && -d "$dir/$name-etc" ]] || exit 65
    gitlab-ctl stop puma
    gitlab-ctl stop sidekiq
    apt-mark unhold "$pkg" || true
    dpkg -r "$pkg"
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$pkg=$ver"
    apt-mark hold "$pkg"
    mv /etc/gitlab "/etc/gitlab.usm-$(date +%s)"
    tar -xf "$etc_tar" -C /
    gitlab-ctl reconfigure
    gitlab-ctl stop puma
    gitlab-ctl stop sidekiq
    GITLAB_ASSUME_YES=1 gitlab-backup restore
    gitlab-ctl reconfigure
    gitlab-ctl restart
    ;;
esac
