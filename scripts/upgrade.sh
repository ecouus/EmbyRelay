#!/usr/bin/env bash
set -euo pipefail
command -v python3 >/dev/null || { echo '需要 Python 3.9+，请先安装 python3。' >&2; exit 2; }
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ -f "$script_dir/upgrade.py" ]]; then
  exec python3 "$script_dir/upgrade.py" "$@"
fi
# A downloaded entry point gets the public implementation over verified HTTPS.
umask 077
upgrade_tmp=$(mktemp -d /var/tmp/embyrelay-upgrade.XXXXXX)
command -v curl >/dev/null || { echo '需要 curl。' >&2; exit 2; }
if ! curl --proto '=https' --tlsv1.2 -fsSL --retry 3 \
  https://raw.githubusercontent.com/ecouus/EmbyRelay/main/scripts/upgrade.py -o "$upgrade_tmp/upgrade.py"; then
  echo '公共升级器下载失败；部署未修改。' >&2
  rm -rf -- "$upgrade_tmp"
  exit 3
fi
# On failure retain the implementation referenced by recovery commands.
if python3 "$upgrade_tmp/upgrade.py" "$@"; then
  rm -rf -- "$upgrade_tmp"
else
  status=$?
  echo "升级器保留在 $upgrade_tmp/upgrade.py" >&2
  exit "$status"
fi
