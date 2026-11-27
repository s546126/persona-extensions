#!/usr/bin/env bash
# ats METHOD PATH [JSON_BODY]: one call to the ATS extension's API. The assistant's gate decides whether the
# call may go out (reads pass, queueing a task asks the user) and adds the credential.
set -euo pipefail
if [ $# -lt 2 ]; then
  echo "usage: ats METHOD PATH [JSON_BODY]" >&2
  exit 2
fi
method=$1
path=$2
shift 2
if [ $# -ge 1 ]; then
  if [[ $1 == @* ]]; then
    body_file=${1#@}
    if [ ! -f "$body_file" ] || [ ! -r "$body_file" ]; then
      echo "ats: body file does not exist or is not readable: $body_file" >&2
      exit 1
    fi
    exec curl -sS -X "$method" "${ATS_BASE:?}$path" -H 'Content-Type: application/json' --data-binary "@$body_file"
  fi
  exec curl -sS -X "$method" "${ATS_BASE:?}$path" -H 'Content-Type: application/json' --data-raw "$1"
fi
exec curl -sS -X "$method" "${ATS_BASE:?}$path"
