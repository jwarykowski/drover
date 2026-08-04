#!/bin/sh
# A source is a process, not a type — so a scheduled event needs no drover code
# at all. This is the whole timer source.
#
#   [[source]]
#   name = "nightly"
#   cmd  = ["/path/to/timer.sh"]
#   types = ["timer.tick"]
#
# Env: TYPE (event type), SUBJECT (the one-run-at-a-time key), EVERY (seconds).
#
# The id carries the tick so each fire is a distinct event; the subject stays
# stable so a run that overruns its interval never stacks a second one.
#
# ponytail: a sleep loop drifts and does not fire ticks missed while the machine
# was asleep or drover was down. If either matters, drop this and let cron or
# launchd schedule a POST to a `drover source webhook` address instead — the OS
# schedulers already solve both.
set -eu
: "${TYPE:=timer.tick}" "${SUBJECT:=nightly}" "${EVERY:=86400}"

while :; do
	printf '{"id":"timer:%s:%s","type":"%s","data":{"title":"%s tick","subject":"%s"}}\n' \
		"$SUBJECT" "$(date +%s)" "$TYPE" "$SUBJECT" "$SUBJECT"
	sleep "$EVERY"
done
