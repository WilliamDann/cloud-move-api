#!/bin/sh
# Fake cql binary for tests. Understands enough of the real invocation to
# exercise the runner and parser: it writes a canned output pgn (with
# cqlmatch markers) to the -o target, optionally after a delay.
prev=""
out=""
for arg in "$@"; do
    case "$prev" in
        -o) out="$arg" ;;
        -i) [ -n "$FAKE_INPUT_COPY" ] && cp "$arg" "$FAKE_INPUT_COPY" ;;
    esac
    case "$arg" in
        -o|-i|-matchstring) prev="$arg" ;;
        *) prev="" ;;
    esac
done

[ -n "$FAKE_SLEEP" ] && sleep "$FAKE_SLEEP"

# emit a controlled diagnostic before failing, to exercise the
# input-error vs engine-error classification
[ -n "$FAKE_STDOUT" ] && printf '%s' "$FAKE_STDOUT"

if [ -n "$FAKE_EXIT_CODE" ]; then
    exit "$FAKE_EXIT_CODE"
fi

cat > "${out:-/dev/null}" <<'PGN'
[Event "F/S Return Match"]
[White "Fischer, Robert J."]
[Black "Spassky, Boris V."]
[Result "1/2-1/2"]

1. {cqlmatch} e4 e5 2. Nf3 {cqlmatch} Nc6 3. Bb5 1/2-1/2

[White "AlphaZero"]
[Black "Stockfish 8"]
[Date "2017.12.06"]
[Result "1-0"]

1. e4 {cqlmatch} e5 1-0

PGN
