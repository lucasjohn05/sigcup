# Windows version of run_sim.sh. Your view goes here (see YOU_FILL_IN.md, step 3).
# Run with:  powershell -ExecutionPolicy Bypass -File .\run_sim.ps1
# ---------------------------------------------------------------------------
$SHIFT   = ""      # >>> YOU: points better for Dems than markets price (e.g. 0, 1.5, 3). Negative = GOP.
$TARGET  = ""      # >>> YOU: final balance that wins 1st, as a multiple of bankroll (e.g. 2).
$RESERVE = "0.3"   # >>> YOU: fraction kept in cash for election night.
$MAX_BET = "5000"  # >>> YOU: cap per contract in SUSQies. Revisit after 2-3 days of depth.csv.
# ---------------------------------------------------------------------------
if ($SHIFT -eq "" -or $TARGET -eq "") {
    Write-Error "Set SHIFT and TARGET at the top of run_sim.ps1 first (see YOU_FILL_IN.md, step 3)."
    exit 1
}
& .\sim.exe -shift $SHIFT -target $TARGET -reserve $RESERVE -max-bet $MAX_BET @args
