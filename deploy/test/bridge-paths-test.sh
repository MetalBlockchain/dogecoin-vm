#!/usr/bin/env bash
# Checks deploy/bridge-paths.sh reads the bridge unit's files, with and
# without explicit flags. Run from the repo root: deploy/test/bridge-paths-test.sh
set -uo pipefail
SCRIPT=$(cd "$(dirname "$0")/.." && pwd)/bridge-paths.sh
pass=0 fail=0
expect() { if eval "$1"; then echo "  ok: $2"; pass=$((pass + 1)); else echo "  FAIL: $2"; fail=$((fail + 1)); fi; }
T=$(mktemp -d)
mkdir -p "$T/bin"
cat >"$T/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
echo "{ path=/opt/dogevm/bin/dogevm ; argv[]=/opt/dogevm/bin/dogevm bridge $UNIT_ARGS ; ignore_errors=no ; }"
STUB
chmod +x "$T/bin/systemctl"
run() { UNIT_ARGS=$1 PATH="$T/bin:$PATH" bash "$SCRIPT"; }

echo "1. explicit flags, as after move-web-state.sh"
eval "$(run '-signers /s/separate-signers/signers.json -deposits /state/web/deposits.json -cosigners /s/separate-signers/cosigners.json -doge-fee 10000000')"
expect '[[ $SET == /s/separate-signers/signers.json ]]' "SET from -signers"
expect '[[ $REG == /state/web/deposits.json ]]' "REG from -deposits, not next to SET"
expect '[[ $COS == /s/separate-signers/cosigners.json ]]' "COS from -cosigners"

echo "2. no -deposits: next to the signer set, as dogevm reads it"
eval "$(run '-signers /s/separate-signers/signers.json')"
expect '[[ $REG == /s/separate-signers/deposits.json ]]' "default registry"

echo "3. no -signers: refuses"
expect '! run "-deposits /x" >/dev/null 2>&1' "fails"
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
