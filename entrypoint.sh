#!/bin/bash
set -uo pipefail

# The display Chrome runs headed on, since Turnstile refuses a headless Chrome.
# Xvfb writes its display number to the descriptor once it is ready for clients.
display_file=$(mktemp)
Xvfb -screen 0 1280x720x24 -nolisten tcp -displayfd 3 3>"${display_file}" &
xvfb=$!

for _ in $(seq 1 100); do
  [ -s "${display_file}" ] && break
  sleep 0.1
done
if ! [ -s "${display_file}" ]; then
  echo "Xvfb did not come up." >&2
  exit 1
fi
export DISPLAY=":$(cat "${display_file}")"

/app "$@" &
app=$!

trap 'kill -TERM "${app}" "${xvfb}" 2>/dev/null' TERM INT

# Either ending ends the container, and the unit starts it again whole: an
# updater whose display has gone would only fail every update.
wait -n "${app}" "${xvfb}"
status=$?
kill -TERM "${app}" "${xvfb}" 2>/dev/null
wait
exit "${status}"
