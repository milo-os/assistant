#!/usr/bin/env bash
# Runs a typecheck command and passes only when it reports exactly the known
# errors below, so a new type error fails the job and so does fixing one.
#
# Usage: typecheck-known-errors.sh npm run typecheck
#
# The known errors: chat-dock.tsx passes onChoose, onApprovePlan and
# onChangePlan, which exist in assistant-chat-kit 0.2.1, but npm only has
# 0.2.0. Once 0.2.1 is on npm and the lockfile is bumped the typecheck comes
# back clean, and this script fails on purpose: delete it and run the
# typecheck directly. Editing chat-dock.tsx can move these positions; update
# the list only after checking that each entry is still one of these props.
set -euo pipefail

# Each entry is "file(line,col) Property": the position and the prop, without
# the type text TypeScript prints after it.
KNOWN_ERRORS="$(cat <<'EOF'
src/widgets/chat-dock.tsx(258,35) onChoose
src/widgets/chat-dock.tsx(259,40) onApprovePlan
src/widgets/chat-dock.tsx(260,39) onChangePlan
src/widgets/chat-dock.tsx(327,29) onChoose
src/widgets/chat-dock.tsx(328,34) onApprovePlan
src/widgets/chat-dock.tsx(329,33) onChangePlan
EOF
)"

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <typecheck command...>" >&2
  exit 2
fi

status=0
output="$("$@" 2>&1)" || status=$?
printf '%s\n' "$output"

if [ "$status" -eq 0 ]; then
  echo "::error::typecheck is clean, so the known chat-dock.tsx errors are gone: delete .github/scripts/typecheck-known-errors.sh and run the typecheck directly" >&2
  exit 1
fi

# Every error line, reduced to "file(line,col) Property" for the known kind and
# kept whole for anything else, so any other error shows up as unexpected.
actual="$(printf '%s\n' "$output" | grep -E 'error TS[0-9]+' \
  | sed -E "s/^([^ ]+\([0-9]+,[0-9]+\)): error TS2339: Property '([A-Za-z]+)' does not exist on type .*/\1 \2/" \
  | sort || true)"
expected="$(printf '%s\n' "$KNOWN_ERRORS" | sort)"

if [ "$actual" != "$expected" ]; then
  echo "::error::typecheck errors differ from the known list (exit $status)" >&2
  echo "Not in the known list:" >&2
  comm -13 <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2
  echo "Known but not reported:" >&2
  comm -23 <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2
  exit 1
fi
echo "::warning::typecheck reports only the 6 known chat-dock.tsx errors (assistant-chat-kit 0.2.1 is not on npm yet)"
