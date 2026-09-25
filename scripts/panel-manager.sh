#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly REPOSITORY="https://github.com/kexue-aihao/Traffic-Forwarding-Panel"
readonly DEFAULT_INSTALL_DIR="/opt/traffic-forwarding-panel"

install_dir="${TFP_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
admin="${TFP_ADMIN:-admin}"
port=""
bundle=""
password_stdin=false
delete_data=false
assume_yes=false
action=""
installer_path=""
installer_temp_path=""

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'HELP'
用法：sudo bash panel-manager.sh [操作] [选项]

操作：
  menu                         打开交互式管理菜单（默认）
  install                      安装服务（首次安装）
  upgrade                      升级到最新正式版（自动校验并备份）
  reset-password               重置管理员或用户密码
  uninstall                    卸载服务，默认保留数据

选项：
  --dir DIR                    安装目录（默认 /opt/traffic-forwarding-panel）
  --admin USER                 管理员或用户账号（默认 admin）
  --port PORT                  首次安装的宿主机回环端口
  --bundle DIR                 使用离线 Docker 发布包安装或升级
  --password-stdin             首次安装从标准输入读取密码
  --delete-data                卸载时同时删除安装目录和数据库
  -y, --yes                    跳过卸载确认（删除数据仍需显式 --delete-data）
  -h, --help                   显示帮助

示例：
  sudo bash panel-manager.sh
  sudo bash panel-manager.sh install --dir /opt/traffic-forwarding-panel
  sudo bash panel-manager.sh upgrade
  sudo bash panel-manager.sh reset-password --admin admin
  sudo bash panel-manager.sh uninstall
  sudo bash panel-manager.sh uninstall --delete-data --yes
HELP
}

parse_args() {
    while (($#)); do
        case "$1" in
            menu|install|upgrade|reset-password|uninstall)
                [[ -z $action ]] || die "不能同时指定多个操作：$1"
                action="$1"
                shift
                ;;
            --dir|--admin|--port|--bundle)
                (($# >= 2)) || die "$1 缺少参数"
                case "$1" in
                    --dir) install_dir=$2 ;;
                    --admin) admin=$2 ;;
                    --port) port=$2 ;;
                    --bundle) bundle=$2 ;;
                esac
                shift 2
                ;;
            --password-stdin) password_stdin=true; shift ;;
            --delete-data|--purge-data) delete_data=true; shift ;;
            -y|--yes) assume_yes=true; shift ;;
            -h|--help) usage; exit 0 ;;
            *) die "未知参数：$1" ;;
        esac
    done
}

require_root() {
    ((EUID == 0)) || die '请使用 sudo bash panel-manager.sh'
}

validate_install_dir() {
    [[ $install_dir =~ ^/([a-zA-Z0-9_.-]+/)*[a-zA-Z0-9_.-]+$ ]] || \
        die '安装目录必须是绝对路径且不含空格'
    [[ $install_dir != *'/../'* && $install_dir != */.. && $install_dir != *'/./'* && $install_dir != */. ]] || \
        die '安装目录不能包含 . 或 .. 路径段'
    [[ ! -L $install_dir ]] || die '安装目录不能是符号链接'
    case "$install_dir" in
        /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
            die '拒绝操作系统目录，请指定具体的面板安装目录'
            ;;
    esac
}

require_docker() {
    command -v docker >/dev/null 2>&1 || die '未找到 Docker'
    docker info >/dev/null 2>&1 || die 'Docker 未运行'
    docker compose version >/dev/null 2>&1 || die '需要 Docker Compose v2'
}

require_install() {
    [[ -d $install_dir ]] || die "安装目录不存在：$install_dir"
    [[ -f $install_dir/compose.yaml && -f $install_dir/.env ]] || \
        die "安装目录缺少 compose.yaml 或 .env：$install_dir"
}

compose() {
    (cd "$install_dir" && docker compose "$@")
}

panel_running() {
    local container
    container=$(compose ps -q panel 2>/dev/null || true)
    [[ -n $container ]] || return 1
    [[ $(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null || true) == true ]]
}

find_installer() {
    local prefer_latest=${1:-false} script_dir candidate temp
    installer_path=""
    installer_temp_path=""
    script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
    candidate=""
    if [[ -n $bundle && -f $bundle/install-docker.sh ]]; then
        candidate="$bundle/install-docker.sh"
    elif [[ -f $script_dir/install-docker.sh ]]; then
        candidate="$script_dir/install-docker.sh"
    fi
    # A local installer is only suitable for an offline bundle. For online
    # installs and upgrades, always fetch the latest release so an old copy
    # next to this manager cannot pin the deployment to an old version.
    if ! $prefer_latest && [[ -n $candidate ]]; then
        installer_path=$candidate
        return
    fi

    command -v curl >/dev/null 2>&1 || die '未找到 curl，无法下载安装器'
    temp=$(mktemp)
    if ! curl --fail --show-error --silent --location --retry 3 --connect-timeout 20 \
        "$REPOSITORY/releases/latest/download/install-docker.sh" -o "$temp"; then
        rm -f -- "$temp"
        return 1
    fi
    chmod 700 "$temp"
    installer_path=$temp
    installer_temp_path=$temp
}

run_installer() {
    local installer installer_temp="" result
    local -a args
    if [[ -n $bundle ]]; then
        # Offline bundles contain a matching installer and image version.
        find_installer false || return $?
    else
        # Online installs and upgrades must use the latest release.
        find_installer true || return $?
    fi
    installer=$installer_path
    installer_temp=$installer_temp_path
    args=(--dir "$install_dir")
    [[ -n $port ]] && args+=(--port "$port")
    [[ $admin != admin ]] && args+=(--admin "$admin")
    [[ -n $bundle ]] && args+=(--bundle "$bundle")
    $password_stdin && args+=(--password-stdin)
    if bash "$installer" "${args[@]}"; then
        :
    else
        result=$?
        [[ -z $installer_temp ]] || rm -f -- "$installer_temp"
        return "$result"
    fi
    [[ -z $installer_temp ]] || rm -f -- "$installer_temp"
}

reset_password() {
    require_docker
    require_install
    printf '请输入账号 %s 的新密码（12–72 个字符，输入内容不会写入命令历史）：\n' "$admin"
    if panel_running; then
        compose exec -T panel /panel -reset-password "$admin"
    else
        compose run --rm -T --no-deps panel -reset-password "$admin"
    fi
}

confirm() {
    local prompt=$1 answer
    $assume_yes && return 0
    [[ -t 0 ]] || die '非交互模式需要使用 --yes'
    read -r -p "$prompt [y/N] " answer
    [[ $answer =~ ^[Yy]$ ]]
}

uninstall() {
    require_docker
    require_install
    if $delete_data; then
        confirm "将停止服务并永久删除 $install_dir（包含数据库），继续吗？" || {
            printf '已取消。\n'
            return 0
        }
    else
        confirm "将停止并卸载面板服务，保留 $install_dir 中的数据，继续吗？" || {
            printf '已取消。\n'
            return 0
        }
    fi

    compose rm -f -s panel
    if $delete_data; then
        rm -rf -- "$install_dir"
        printf '服务和安装目录已删除。\n'
    else
        printf '服务已卸载，数据和配置保留在：%s\n' "$install_dir"
        printf '之后可使用同一目录再次安装或升级。\n'
    fi
}

menu() {
    local choice username
    while true; do
        printf '\nTraffic-Forwarding-Panel 管理\n'
        printf '安装目录：%s\n' "$install_dir"
        printf '  1) 安装服务\n'
        printf '  2) 升级到最新正式版\n'
        printf '  3) 重置密码\n'
        printf '  4) 卸载服务（保留数据）\n'
        printf '  5) 卸载服务并删除数据\n'
        printf '  0) 退出\n'
        read -r -p '请选择 [0-5]：' choice || return 0
        case "$choice" in
            1) run_installer ;;
            2) run_installer ;;
            3)
                read -r -p "账号 [$admin]：" username
                [[ -z $username ]] || admin=$username
                reset_password
                ;;
            4) delete_data=false; uninstall ;;
            5) delete_data=true; uninstall ;;
            0) return 0 ;;
            *) printf '无效选项。\n' ;;
        esac
    done
}

parse_args "$@"
require_root
validate_install_dir

if [[ -z $action ]]; then
    [[ -t 0 ]] || { usage; exit 0; }
    action=menu
fi

case "$action" in
    menu) menu ;;
    install|upgrade) run_installer ;;
    reset-password) reset_password ;;
    uninstall) uninstall ;;
esac
