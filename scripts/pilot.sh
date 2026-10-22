#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

case "${1:-}" in
  up)
    exec docker compose -f compose.pilot.yml -p bvk-pilot up -d --wait
    ;;
  status)
    exec docker compose -f compose.pilot.yml -p bvk-pilot ps
    ;;
  migrate|check|run|backup|analytics-migrate|analytics-once|analytics-run)
    if [ ! -f data/pilot/runtime.env ] || [ ! -f data/pilot/config.json ]; then
      echo 'Pilot environment is missing in data/pilot/' >&2
      exit 1
    fi
    set -a
    . ./data/pilot/runtime.env
    if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] && [ -f data/pilot/telegram.env ]; then
      . ./data/pilot/telegram.env
    fi
    set +a
    case "$DATABASE_URL" in
      *'@127.0.0.1:55434/bvk_pilot?sslmode=disable') ;;
      *) echo 'Refusing to use a database other than the pilot database' >&2; exit 1 ;;
    esac
    case "$1" in
      check|run)
        if [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then
          echo 'Set TELEGRAM_BOT_TOKEN in this terminal before checking or running the bot' >&2
          exit 1
        fi
        ;;
    esac
    case "$1" in
      migrate) exec ./bin/bvk -config data/pilot/config.json -migrate ;;
      check) exec ./bin/bvk -config data/pilot/config.json -check ;;
      run) exec ./bin/bvk -config data/pilot/config.json ;;
      backup) exec ./bin/bvk -config data/pilot/config.json -backup ;;
      analytics-migrate) exec ./bin/bvk-analytics -config data/pilot/config.json -migrate ;;
      analytics-once) exec ./bin/bvk-analytics -config data/pilot/config.json -once ;;
      analytics-run) exec ./bin/bvk-analytics -config data/pilot/config.json ;;
    esac
    ;;
  *)
    echo 'Usage: scripts/pilot.sh {up|status|migrate|check|run|backup|analytics-migrate|analytics-once|analytics-run}' >&2
    exit 2
    ;;
esac
