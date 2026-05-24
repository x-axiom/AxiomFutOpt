#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://www.cffex.com.cn}"
HTML_FILE="${1:-data.html}"
DOWNLOAD_DIR="${2:-downloads}"
EXTRACT_DIR="${3:-extracted}"

if [[ ! -f "$HTML_FILE" ]]; then
  echo "HTML file not found: $HTML_FILE" >&2
  exit 1
fi

mkdir -p "$DOWNLOAD_DIR" "$EXTRACT_DIR"

href_list="$(mktemp)"
trap 'rm -f "$href_list"' EXIT

grep -oE 'href="[^"]+\.zip"' "$HTML_FILE" \
  | sed -E 's/^href="([^"]+)"$/\1/' \
  | awk '!seen[$0]++' > "$href_list"

if [[ ! -s "$href_list" ]]; then
  echo "No zip links found in $HTML_FILE" >&2
  exit 1
fi

while IFS= read -r relative_url; do
  [[ -n "$relative_url" ]] || continue

  if [[ "$relative_url" =~ ^https?:// ]]; then
    url="$relative_url"
  else
    url="${BASE_URL%/}/${relative_url#/}"
  fi

  file_name="${url##*/}"
  extract_subdir="${file_name%.zip}"
  zip_path="$DOWNLOAD_DIR/$file_name"
  target_dir="$EXTRACT_DIR/$extract_subdir"

  if [[ -s "$zip_path" ]]; then
    echo "Skip download: $zip_path"
  else
    echo "Downloading $url"
    curl --fail --location --retry 3 --output "$zip_path" "$url"
  fi

  mkdir -p "$target_dir"
  echo "Extracting $zip_path -> $target_dir"
  unzip -o -q "$zip_path" -d "$target_dir"
done < "$href_list"

echo "Done. Downloads: $DOWNLOAD_DIR Extracted: $EXTRACT_DIR"