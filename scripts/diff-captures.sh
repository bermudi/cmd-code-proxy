#!/usr/bin/env bash
# Compare proxy captures against real binary captures from cmd-recorder.
#
# Usage:
#   diff-captures.sh <proxy-capture-dir> <cmd-recorder-dir>
#
# Finds the latest proxy .request.json and the latest cmd-recorder
# POST_alpha_generate.json, normalizes both with jq, and runs diff.
#
# Example:
#   diff-captures.sh ./captures ../cmd-recorder/captures

set -euo pipefail

PROXY_DIR="${1:-}"
RECORDER_DIR="${2:-}"

if [[ -z "$PROXY_DIR" || -z "$RECORDER_DIR" ]]; then
    echo "Usage: $0 <proxy-capture-dir> <cmd-recorder-dir>"
    exit 1
fi

# Find latest proxy .request.json
proxy_file=$(find "$PROXY_DIR" -maxdepth 1 -name '*.request.json' -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-)
if [[ -z "$proxy_file" ]]; then
    echo "No proxy .request.json files found in $PROXY_DIR"
    exit 1
fi

# Find latest cmd-recorder POST_alpha_generate.json
recorder_file=$(find "$RECORDER_DIR" -maxdepth 1 -name '*POST_alpha_generate.json' -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-)
if [[ -z "$recorder_file" ]]; then
    echo "No cmd-recorder POST_alpha_generate.json files found in $RECORDER_DIR"
    exit 1
fi

echo "Proxy:     $proxy_file"
echo "Recorder:  $recorder_file"
echo ""

# Normalize both files with jq (sort keys, compact arrays)
# Thread the output through a process substitution so diff works on the fly
proxy_norm=$(mktemp)
recorder_norm=$(mktemp)
trap "rm -f '$proxy_norm' '$recorder_norm'" EXIT

jq -S '.' "$proxy_file" > "$proxy_norm"
jq -S '.' "$recorder_file" > "$recorder_norm"

# Run diff
diff -u "$recorder_norm" "$proxy_norm" || true

echo ""
echo "--- Summary of known expected differences ---"
echo "memory:     proxy sends null; real binary sends AGENTS.md content"
echo "taste:      proxy sends null; real binary sends .commandcode/taste content"
echo "skills:     proxy sends empty string; real binary sends XML from .commandcode/skills/"
echo "threadId:   proxy omits; real binary sends session UUID"
echo "config:     should match exactly (workingDir, environment, structure, git fields)"
echo "params:     should match exactly (model, messages, tools, max_tokens, stream)"
