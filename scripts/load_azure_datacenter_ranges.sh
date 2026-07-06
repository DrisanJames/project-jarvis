#!/usr/bin/env bash
# =============================================================================
# load_azure_datacenter_ranges.sh — bulk-load the FULL AzureCloud ServiceTags
# snapshot into ignite_datacenter_ranges via COPY.
#
# This is deliberately OUT-OF-BAND from runStartupMigrations(): the full snapshot
# is thousands of prefixes (IPv4 + IPv6) and inlining it into the 5s-per-statement
# migration slice would blow the budget and silently roll back (2026-06-10 class
# of footgun). The startup migration only seeds a small bootstrap set; this script
# fills in the rest and is safe to re-run (idempotent upsert on the cidr PK).
#
# Usage:
#   1. Download the current ServiceTags file (updated weekly by Microsoft):
#        https://www.microsoft.com/en-us/download/details.aspx?id=56519
#      Save the JSON, e.g. ServiceTags_Public_YYYYMMDD.json
#   2. Run:
#        DATABASE_URL=postgres://user:pass@host:5432/db \
#          bash scripts/load_azure_datacenter_ranges.sh ServiceTags_Public_YYYYMMDD.json
#      Optional: pass a service tag prefix to load (default: AzureCloud, which is
#      the top-level aggregate covering all regions):
#        ... load_azure_datacenter_ranges.sh <file.json> AzureCloud
#
# Requires: jq, psql. NEVER run against prod without confirming DATABASE_URL — a
# read-only diff mode is provided (DRY_RUN=1) that only prints the row count.
# =============================================================================
set -euo pipefail

JSON_FILE="${1:?usage: load_azure_datacenter_ranges.sh <ServiceTags.json> [service_tag_prefix]}"
TAG_PREFIX="${2:-AzureCloud}"
: "${DATABASE_URL:?set DATABASE_URL to the target Postgres}"

command -v jq   >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v psql >/dev/null || { echo "psql is required" >&2; exit 1; }

TMP_CSV="$(mktemp -t azure_ranges.XXXXXX.csv)"
trap 'rm -f "$TMP_CSV"' EXIT

# Extract every addressPrefix under service tags whose name starts with the
# requested prefix (AzureCloud, AzureCloud.eastus, ...). One CIDR per line; the
# provider/service_tag/source columns are constant. Postgres will reject any
# malformed CIDR at COPY time, which is the validation we want.
jq -r --arg tag "$TAG_PREFIX" '
  .values[]
  | select(.name | startswith($tag))
  | .properties.addressPrefixes[]
  | [ ., "microsoft", "AzureCloud", "azure_servicetags" ]
  | @csv
' "$JSON_FILE" | sort -u > "$TMP_CSV"

ROW_COUNT="$(wc -l < "$TMP_CSV" | tr -d ' ')"
echo "[load_azure] extracted $ROW_COUNT unique prefixes for tag '$TAG_PREFIX' from $JSON_FILE"

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  echo "[load_azure] DRY_RUN=1 — not writing. Sample:"; head -5 "$TMP_CSV"
  exit 0
fi

# Stage into a temp table, then upsert so re-runs refresh updated_at without
# disturbing first_seen. The COPY + single INSERT..SELECT is one fast pass.
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE _azure_stage (
  cidr cidr, provider text, service_tag text, source text
) ON COMMIT DROP;

\copy _azure_stage (cidr, provider, service_tag, source) FROM '$TMP_CSV' WITH (FORMAT csv)

INSERT INTO ignite_datacenter_ranges (cidr, provider, service_tag, source)
SELECT cidr, provider, service_tag, source FROM _azure_stage
ON CONFLICT (cidr) DO UPDATE
  SET provider   = EXCLUDED.provider,
      service_tag = EXCLUDED.service_tag,
      source     = EXCLUDED.source,
      updated_at = now();

SELECT count(*) AS total_ranges,
       count(*) FILTER (WHERE family(cidr) = 6) AS ipv6_ranges
FROM ignite_datacenter_ranges;
SQL

echo "[load_azure] done."
