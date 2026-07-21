#!/bin/sh
# Minimal UCI engine for tests. Records each spawn's pid so tests can count
# process launches; ignores argv (the api appends --model <name>).
[ -n "$FAKE_SPAWN_LOG" ] && echo $$ >> "$FAKE_SPAWN_LOG"

while read -r line; do
    case "$line" in
        uci)
            echo "id name fake-uci"
            echo "uciok"
            ;;
        isready)
            echo "readyok"
            ;;
        go*)
            echo "info multipv 1 score cp 12 wdl 310 400 290 pv e2e4 e7e5"
            echo "info multipv 2 score cp 5 pv d2d4 d7d5"
            echo "bestmove e2e4"
            ;;
        quit)
            exit 0
            ;;
    esac
done
