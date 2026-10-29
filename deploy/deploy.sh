#!/bin/sh
# Manual rollback uses the same supervised two-slot switch as an update.
set -eu
deployment_directory=$(CDPATH='' cd -P "$(dirname "$0")" && pwd)
case "${1:-}" in
  back) exec "$deployment_directory/install-proxy.sh" --back ;;
  *) echo 'usage: deploy/deploy.sh back' >&2; exit 2 ;;
esac
