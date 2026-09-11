#!/usr/bin/env bash
# check-content-desk-contract.sh — the Content Desk contract goldens live in
# THREE places; exit 1 if either copy drifts from the backend's (sha256).
#
#   backend   internal/contentdesk/testdata/contract/        source of truth, written by
#             CONTENT_DESK_CONTRACT_UPDATE=1 go test -tags integration -run ContentDeskContract ./cmd/server/
#   portal    web/src/components/mailing/components/__fixtures__/content-desk/
#   publisher <mailing-saas>/agents/jobs/tests/fixtures/content_desk_contract/
#
# --sync  copy the backend set over the other two first.
# CONTENT_DESK_PUBLISHER_FIXTURES overrides the publisher dir (e.g. from a worktree).
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/internal/contentdesk/testdata/contract"
web="$root/web/src/components/mailing/components/__fixtures__/content-desk"
pub="${CONTENT_DESK_PUBLISHER_FIXTURES:-$root/../agents/jobs/tests/fixtures/content_desk_contract}"

if [[ "${1:-}" == "--sync" ]]; then
  for d in "$web" "$pub"; do
    mkdir -p "$d"
    rm -f "$d"/*.json
    cp "$src"/*.json "$d"/
  done
fi

sums() { (cd "$1" && shasum -a 256 -- *.json); }
want="$(sums "$src")"
rc=0
for d in "$web" "$pub"; do
  if [[ ! -d "$d" ]]; then
    echo "MISSING $d"
    rc=1
    continue
  fi
  got="$(sums "$d" 2>/dev/null || true)"
  if [[ "$got" != "$want" ]]; then
    echo "DRIFT   $d"
    diff <(echo "$want") <(echo "$got") || true
    rc=1
  else
    echo "OK      $d ($(echo "$want" | wc -l | tr -d ' ') files)"
  fi
done
exit $rc
