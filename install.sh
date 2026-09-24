#!/usr/bin/env bash
# Small entry point for curl | bash; run the complete release script from a file.
set -euo pipefail

install_panel() (
    umask 077
    installer=$(mktemp)
    trap 'rm -f -- "$installer"' EXIT
    curl --fail --show-error --silent --location --retry 3 --connect-timeout 20 \
        https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/latest/download/install-docker.sh \
        -o "$installer"
    # Keep Docker and the downloaded script off the pipe carrying this launcher.
    if ((EUID == 0)); then
        bash "$installer" "$@" </dev/null
    else
        sudo bash "$installer" "$@" </dev/null
    fi
)

install_panel "$@"
