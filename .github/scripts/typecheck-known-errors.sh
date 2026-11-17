#!/usr/bin/env bash
# Runs a typecheck command and passes only when it is clean or when every
# error it reports is one of the 6 known TS2339 errors in
# src/widgets/chat-dock.tsx, so new type errors still fail the job.
#
# Usage: typecheck-known-errors.sh npm run typecheck
#
# The known errors: chat-dock.tsx passes onChoose, onApprovePlan and
# onChangePlan, which exist in assistant-chat-kit 0.2.1, but npm only has
# 0.2.0. Delete this script and run the typecheck directly once 0.2.1 is on npm
# and the lockfile is bumped.
set -euo pipefail

KNOWN_COUNT=6
KNOWN_ERROR="^src/widgets/chat-dock\.tsx\([0-9]+,[0-9]+\): error TS2339: Property '(onChoose|onApprovePlan|onChangePlan)' does not exist on type "

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <typecheck command...>" >&2
  exit 2
fi

status=0
output="$("$@" 2>&1)" || status=$?
printf '%s\n' "$output"

if [ "$status" -eq 0 ]; then
  echo "typecheck is clean: the known chat-dock.tsx errors are gone, so run it directly now."
  exit 0
fi

errors="$(printf '%s\n' "$output" | grep -E 'error TS[0-9]+' || true)"
unexpected="$(printf '%s\n' "$errors" | grep -vE "$KNOWN_ERROR" | grep -v '^$' || true)"
count="$(printf '%s\n' "$errors" | grep -cE "$KNOWN_ERROR" || true)"

if [ -n "$unexpected" ]; then
  echo "::error::typecheck reported errors beyond the known chat-dock.tsx ones:" >&2
  printf '%s\n' "$unexpected" >&2
  exit 1
fi
if [ "$count" -ne "$KNOWN_COUNT" ]; then
  echo "::error::typecheck failed (exit $status) with $count of the $KNOWN_COUNT known errors, not exactly them" >&2
  exit 1
fi
echo "::warning::typecheck reports only the $KNOWN_COUNT known chat-dock.tsx errors (assistant-chat-kit 0.2.1 is not on npm yet)"
