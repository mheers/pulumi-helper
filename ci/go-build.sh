#!/usr/bin/env bash

: '
This script prepares the flags (git-tag, go version, goos) and runs the build command.
'

set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" ; pwd -P)"
PROJECT="ssp"
PROJECT_PATH=$SCRIPT_DIR"/../../"$PROJECT

NAME_DEFAULT="app"
OS_DEFAULT="linux"
ARCH_DEFAULT="amd64"

function usage() {
  echo "usage: ${0} --name <name> --os [linux|windows|darwin] [--arch <amd64|386|arm>]"
  echo "        name: the name of the binary (prefix) to compile"
  echo "        os: the operating system to compile for"
  echo "        arch: the architecture to compile for"
}

while [[ $# -gt 0 ]]; do
  key="$1"

  case $key in
    --name|-n)
    export NAME="$2"
    shift
    shift
    ;;
    --os|-o)
    export OS="$2"
    shift
    shift
    ;;
    --arch|-a)
    export ARCH="$2"
    shift
    shift
    ;;
    --help|help)
    usage
    exit 0
    ;;
    *)
    shift
    shift
  esac
done

if [[ -z "${NAME}" ]]; then
  echo "no NAME set, using "${NAME_DEFAULT}
  NAME=${NAME_DEFAULT}
fi
if [[ -z "${OS}" ]]; then
  echo "no OS set, using "${OS_DEFAULT}
  OS=${OS_DEFAULT}
fi
if [[ -z "${ARCH}" ]]; then
  echo "no ARCH set, using "${ARCH_DEFAULT}
  ARCH=${ARCH_DEFAULT}
fi

source ${SCRIPT_DIR}/go-version.sh

FLAG="-X $TRG_PKG.BuildTime=$BUILD_TIME"
FLAG="$FLAG -X $TRG_PKG.GitTag=$GitTag"
FLAG="$FLAG -X $TRG_PKG.GitBranch=$GitBranch"
FLAG="$FLAG -X $TRG_PKG.VERSION=$VERSION"

echo -e "Building with flags: "$FLAG

FILE_EXTENSION=""
if [[ ${OS} = "windows" ]]; then
  FILE_EXTENSION=".exe"
fi

CGO_ENABLED=0
GO111MODULE=on CGO_ENABLED=${CGO_ENABLED} GOOS=${OS} GOARCH=${ARCH} go build -a -ldflags "$FLAG" -o goapp

echo -e "Done"
