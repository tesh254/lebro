#!/bin/sh
# Zed is commonly launched without the shell's environment. Load the user's
# interactive zsh configuration while keeping its startup output off stdout.
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 1
exec 3>&1
exec /bin/zsh -ic 'exec 1>&3; export OPENROUTER_API_KEY; exec "$1"' zsh "$script_dir/../../bin/lebro-acp-openrouter" 1>&2
