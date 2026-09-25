#!/usr/bin/env bash
# Unified entry point for first installation, latest-version upgrades and
# password resets. The manager is downloaded from master, then fetches the
# latest release installer for online installs and upgrades.
set -euo pipefail

readonly MANAGER_URL="https://raw.githubusercontent.com/kexue-aihao/Traffic-Forwarding-Panel/master/scripts/panel-manager.sh"

run_manager() (
    umask 077
    manager=$(mktemp)
    trap 'rm -f -- "$manager"' EXIT
    command -v curl >/dev/null 2>&1 || {
        printf '错误：需要 curl 下载最新管理脚本\n' >&2
        exit 1
    }
    curl --fail --show-error --silent --location --retry 3 --connect-timeout 20 \
        "$MANAGER_URL" -o "$manager"
    chmod 700 "$manager"

    # The launcher itself is commonly piped into bash, so use the controlling
    # terminal for the manager menu and its password/confirmation prompts.
    if (($# == 0)); then
        if [[ -r /dev/tty ]]; then
            set -- menu
        else
            printf '错误：无参数启动需要交互式终端；也可显式指定 install、upgrade 或 reset-password。\n' >&2
            exit 1
        fi
    else
        case "$1" in
            install|upgrade|reset-password|uninstall|menu|--help|-h) ;;
            *) set -- install "$@" ;;
        esac
    fi
    if [[ ${1:-} == --help || ${1:-} == -h ]]; then
        bash "$manager" "$@"
    elif [[ ${1:-} == menu || ${1:-} == reset-password || ${1:-} == uninstall ]] && [[ -r /dev/tty ]]; then
        if ((EUID == 0)); then
            bash "$manager" "$@" </dev/tty
        else
            sudo bash "$manager" "$@" </dev/tty
        fi
    elif ((EUID == 0)); then
        bash "$manager" "$@"
    else
        sudo bash "$manager" "$@"
    fi
)

run_manager "$@"
