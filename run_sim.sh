#!/bin/sh
# Your view goes here. See YOU_FILL_IN.md, step 3.
# ---------------------------------------------------------------------------
SHIFT=         # >>> YOU: points better for Dems than markets price (e.g. 0, 1.5, 3). Negative = GOP.
TARGET=        # >>> YOU: final balance that wins 1st, as a multiple of your bankroll (e.g. 2).
RESERVE=0.3    # >>> YOU: fraction kept in cash for election night.
MAX_BET=5000   # >>> YOU: cap per contract in SUSQies. Revisit after 2-3 days of depth.csv.
# ---------------------------------------------------------------------------
if [ -z "$SHIFT" ] || [ -z "$TARGET" ]; then
  echo "Set SHIFT and TARGET at the top of run_sim.sh first (see YOU_FILL_IN.md, step 3)." >&2
  exit 1
fi
exec ./sim -shift "$SHIFT" -target "$TARGET" -reserve "$RESERVE" -max-bet "$MAX_BET" "$@"
