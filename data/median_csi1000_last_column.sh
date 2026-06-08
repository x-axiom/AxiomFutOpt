#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
csv_path=${1:-"$script_dir/csi1000_daily.csv"}
start_date=${2:-""}
end_date=${3:-""}

if [[ ! -f "$csv_path" ]]; then
	echo "csv not found: $csv_path" >&2
	exit 1
fi

header_name=$(head -n 1 "$csv_path" | awk -F',' '{gsub(/\r/, "", $NF); print $NF}')
values_file=$(mktemp)
trap 'rm -f "$values_file"' EXIT

awk -F',' 'NR > 1 {
	gsub(/\r/, "", $1)
	gsub(/\r/, "", $NF)
	if (start_date != "" && $1 < start_date) {
		next
	}
	if (end_date != "" && $1 > end_date) {
		next
	}
	if ($NF != "") {
		print $NF
	}
}' start_date="$start_date" end_date="$end_date" "$csv_path" | sort -g > "$values_file"

count=$(wc -l < "$values_file" | tr -d '[:space:]')
if [[ "$count" == "0" ]]; then
	echo "no numeric values found in last column: $header_name" >&2
	exit 1
fi

if (( count % 2 == 1 )); then
	middle=$((count / 2 + 1))
	median=$(sed -n "${middle}p" "$values_file")
else
	left=$((count / 2))
	right=$((left + 1))
	median=$(awk -v left="$left" -v right="$right" 'NR == left {a = $1} NR == right {b = $1} END {printf "%.10g", (a + b) / 2}' "$values_file")
fi

if [[ -n "$start_date" && -n "$end_date" ]]; then
	printf '%s median from %s to %s: %s\n' "$header_name" "$start_date" "$end_date" "$median"
elif [[ -n "$start_date" ]]; then
	printf '%s median from %s: %s\n' "$header_name" "$start_date" "$median"
elif [[ -n "$end_date" ]]; then
	printf '%s median through %s: %s\n' "$header_name" "$end_date" "$median"
else
	printf '%s median: %s\n' "$header_name" "$median"
fi