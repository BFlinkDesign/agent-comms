#!/usr/bin/env bash
cd "$(dirname "$0")/.."
PYTHON="${PYTHON:-python3}"
"$PYTHON" -m pip install -r dashboard/requirements.txt -q
"$PYTHON" -m dashboard.server
