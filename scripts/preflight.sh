#!/usr/bin/env bash
set -Eeuo pipefail

readonly gib_kib=$((1024 * 1024))
readonly min_total_memory_kib=$((4 * gib_kib))
readonly recommended_total_memory_kib=$((8 * gib_kib))
readonly min_available_memory_kib=$((2 * gib_kib))
readonly recommended_available_memory_kib=$((4 * gib_kib))
readonly min_available_disk_kib=$((12 * gib_kib))
readonly recommended_available_disk_kib=$((20 * gib_kib))

format_gib() {
  awk -v kib="$1" 'BEGIN { printf "%.1f GiB", kib / 1048576 }'
}

case "${VIBEDOCK_SKIP_PREFLIGHT:-}" in
  1|true|TRUE|yes|YES)
    printf 'Skipping capacity checks because VIBEDOCK_SKIP_PREFLIGHT is enabled.\n'
    exit 0
    ;;
esac

probe_output="$({
  docker run --rm --pull=missing alpine:3.22 sh -c '
    while read -r key value unit; do
      case "$key" in
        MemTotal:) printf "memory_total_kib=%s\n" "$value" ;;
        MemAvailable:) printf "memory_available_kib=%s\n" "$value" ;;
      esac
    done </proc/meminfo
    set -- $(df -Pk / | tail -n 1)
    printf "disk_available_kib=%s\n" "$4"
  '
} 2>&1)" || {
  printf 'Unable to inspect Docker capacity:\n%s\n' "$probe_output" >&2
  printf 'Fix Docker access, or bypass only after checking capacity manually with VIBEDOCK_SKIP_PREFLIGHT=1.\n' >&2
  exit 1
}

read_probe_value() {
  local key="$1"
  awk -F= -v key="$key" '$1 == key { print $2; exit }' <<<"$probe_output"
}

memory_total_kib="$(read_probe_value memory_total_kib)"
memory_available_kib="$(read_probe_value memory_available_kib)"
disk_available_kib="$(read_probe_value disk_available_kib)"

for value_name in memory_total_kib memory_available_kib disk_available_kib; do
  value="${!value_name:-}"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    printf 'Docker capacity probe returned an invalid %s value.\n' "$value_name" >&2
    printf 'Probe output:\n%s\n' "$probe_output" >&2
    exit 1
  fi
done

failures=0
warnings=0

report_capacity() {
  local label="$1"
  local actual_kib="$2"
  local minimum_kib="$3"
  local recommended_kib="$4"
  local status

  if (( actual_kib < minimum_kib )); then
    status='FAIL'
    failures=$((failures + 1))
  elif (( actual_kib < recommended_kib )); then
    status='WARN'
    warnings=$((warnings + 1))
  else
    status='OK'
  fi

  printf '  %-25s %9s  [%s]\n' "$label" "$(format_gib "$actual_kib")" "$status"
}

printf '\nChecking Docker host capacity...\n'
report_capacity 'Total memory' "$memory_total_kib" "$min_total_memory_kib" "$recommended_total_memory_kib"
report_capacity 'Available memory' "$memory_available_kib" "$min_available_memory_kib" "$recommended_available_memory_kib"
report_capacity 'Available Docker storage' "$disk_available_kib" "$min_available_disk_kib" "$recommended_available_disk_kib"

printf '  Minimum: 4 GiB total memory, 2 GiB currently available, 12 GiB Docker storage.\n'
printf '  Recommended: 8 GiB total memory, 4 GiB currently available, 20 GiB Docker storage.\n'

if (( failures > 0 )); then
  printf '\nCapacity check failed. Free resources or enlarge the Docker VM/storage before installing VibeDock.\n' >&2
  printf 'To override after checking the machine manually: VIBEDOCK_SKIP_PREFLIGHT=1 ./scripts/bootstrap.sh\n' >&2
  exit 1
fi

if (( warnings > 0 )); then
  printf '\nCapacity is above the minimum but below the recommendation; builds or multiple Workspaces may be slow.\n'
else
  printf 'Capacity check passed.\n'
fi
