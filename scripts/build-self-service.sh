#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$repo_dir"
build_dir="${BUILD_DIR:-dist}"
go_cmd="${GO:-go}"
case "$("$go_cmd" env GOOS)" in
  linux) extension=so ;;
  darwin) extension=dylib ;;
  windows) extension=dll ;;
  *) echo "Unsupported target platform" >&2; exit 1 ;;
esac
mkdir -p "$build_dir"
CGO_ENABLED=1 "$go_cmd" build -buildvcs=false -trimpath -tags cshared -buildmode=c-shared \
  -o "$build_dir/cpa-key-billing.$extension" ./cmd/cpa-key-billing
cp internal/plugin/usage.html "$build_dir/usage.html"
cp internal/plugin/quota.html "$build_dir/quota.html"
python3 - "$build_dir" "$extension" <<'PY'
import hashlib
import sys
from pathlib import Path

root = Path(sys.argv[1])
names = ['usage.html', 'quota.html', 'cpa-key-billing.' + sys.argv[2]]
lines = [hashlib.sha256((root / name).read_bytes()).hexdigest() + '  ' + name for name in names]
(root / 'SHA256SUMS').write_text('\n'.join(lines) + '\n')
print('Built: ' + ', '.join(str(root / name) for name in names))
PY
