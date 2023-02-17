#!/usr/bin/env bash

set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" ; pwd -P)"
. "${SCRIPT_DIR}/helper.inc.sh"

VERSION=$(date +%Y%m%d.%H%M%S)

pushd "${SCRIPT_DIR}/.." > /dev/null

print-banner "Setting new version: ${VERSION}"
echo

echo "export VERSION=${VERSION}" > .VERSION

while [[ $# -gt 0 ]]; do
  key="$1"

  case $key in
    --branch|-n)
    export BRANCH="$2"
    shift
    shift
    ;;
    *)
    shift
    shift
  esac
done

if [[ -z "${BRANCH}" ]]; then
    BRANCH_DEFAULT=$(git rev-parse --abbrev-ref HEAD || echo 'N/A')
    echo "no BRANCH set, using "${BRANCH_DEFAULT}
    BRANCH=${BRANCH_DEFAULT}
fi

print-banner "Setting current branch: ${BRANCH}"

echo "export BRANCH=${BRANCH}" >> .VERSION
