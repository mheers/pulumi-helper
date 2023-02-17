#!/usr/bin/env bash

set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" ; pwd -P)"

BRANCH=N/A
source ${SCRIPT_DIR}/../.VERSION

TRG_PKG='main'
BUILD_TIME=$(date +"%Y%m%d.%H%M%S")
GitTag=N/A
GitBranch=N/A

GV=$(git tag || echo 'N/A')
if [[ $GV =~ [^[:space:]]+ ]];
then
    GitTag=${BASH_REMATCH[0]}
fi

GV=$(git rev-parse --abbrev-ref HEAD || echo ${BRANCH})
if [[ $GV =~ [^[:space:]]+ ]];
then
    if [[ "${BASH_REMATCH[0]}" != "HEAD" ]]; then
        GitBranch=${BASH_REMATCH[0]}
    else
        GitBranch=${BRANCH}
    fi
fi

export TRG_PKG
export BUILD_TIME
export GitTag
export GitBranch
export VERSION
