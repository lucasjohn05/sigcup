# What you fill in

Everything the code can't know is listed here, in the order you need it.
Anything marked `>>> YOU` in a file is yours to set. Nothing else needs
editing.

**On Windows:** build with `.exe` names (`go build -o sigcup.exe ./cmd/sigcup`,
`go build -o sim.exe ./cmd/sim`), run as `.\sigcup.exe ...` and `.\sim.exe ...`,
and use `run_sim.ps1` instead of `run_sim.sh`:
`powershell -ExecutionPolicy Bypass -File .\run_sim.ps1`.

## At a glance

| What | Where | Needed? |
|---|---|---|
| API key, tournament slug | `.env` | yes |
| Phone alerts, heartbeat | `.env` (`ALERT_URL`, `HEARTBEAT_URL`) | strongly recommended |
| Your view: shift, target, reserve, max bet | top of `run_sim.sh` (Windows: `run_sim.ps1`) | yes |
| Independent races (NE/MT Senate, RI Gov) | check rows in `races.csv` | yes, 2 minutes |
| Your own race probabilities | `p_fair` column in `races.csv` | optional |
| The stack | `targets.csv` (the sim writes it; you review it) | yes |
| Kalshi mappings | `mapping.csv` | optional |
| Rating-based fair values | `fair.csv` (copy `fair.example.csv`) | optional |
| Election-night batches | `batches/*.csv` (see `batch.example.csv`) | before Nov 3 |

## Tonight

**1. Build.**
```bash
go build -o sigcup ./cmd/sigcup && go build -o sim ./cmd/sim
cp .env.example .env
```

**2. `.env`.** Paste your trade key and tournament slug. Add `ALERT_URL` (an
ntfy topic) and `HEARTBEAT_URL` (a healthchecks.io check, 5-minute period).
Accept the competition terms in the web UI, or every order fails.

**3. Pick your numbers on the seed board.** No keys needed:
```bash
./sim -races races.seed.csv -offline -shift 2 -target 2
```
Try a few values and read the RESULTS and STRESS tables in
`sim_report.txt`. When you've decided, write them at the top of
`run_sim.sh`.

- **SHIFT: your edge claim, in points.** 0 means markets are right. +2 means
  the environment is 2 points better for Dems than priced (midterm polls
  underestimated Dems in 2018 and 2022). +4 is aggressive. The STRESS table
  shows what the stack does if you're wrong. Look at the 0 row: that's your
  loss if the markets were right all along.
- **TARGET: what you think wins 1st**, as a multiple of your bankroll. Nobody
  knows yet. Start at 2 and revisit once `leaderboard.csv` has a few weeks of
  data.
- **RESERVE: cash kept for election night.** The target, the reserve, and the
  max bet are linked. With 30% reserve and 5k per bet, even a perfect sweep
  lands around 2.6x, so a 3x target is unreachable before election night.
  The sim will show 0% if you ask for the impossible.
- **MAX_BET: per-contract cap.** The seed books hold about 1,000 shares a
  side, so 5k per contract is already big. Revisit after 2–3 days of
  `depth.csv`.

The sim won't bet a side the market prices under 10% (`-min-p`). Those
prices are mostly longshot bias and SIG's price floor, not real chances.

## Tomorrow, once trading opens (noon ET)

**4. Map the board.**
```bash
./sigcup -mode markets            # writes markets_dump.csv
./sim -build markets_dump.csv     # writes races.csv with exchange IDs and live mids
```
Races whose books aren't two-sided yet get their price from the seed board,
and the builder says how many. Any race still without a price is out of play;
put a number in `p_fair` if you want it in. Open `races.csv` and check three
rows against their Info tabs:
- `NE-SEN` and `MT-SEN` should use the **Independent** as the anti-GOP side.
- `RI-GOV` uses the Democrat. GOP NO also pays if the Independent wins.

Optional: put your own probability in `p_fair` for any race where you trust
your number over the market's (e.g. Kalshi, or a rating conversion).

**5. Generate the stack.**
```bash
./run_sim.sh                      # live prices -> sim_report.txt + targets.csv
```
Review every row of `targets.csv`. You can delete rows, lower `max_price`,
or cut `target_shares`. Two things to consider:
- **Florida.** The sim likes several Florida seats, and they're correlated.
  Florida also counts early, so it's your first election-night signal.
  Consider dropping Florida rows and trading them on the night instead.
- **`take_below`.** It's blank by default, which means patient resting bids
  only. Fill it in on a row to let the bot take the ask outright when it's
  at or below that price.

**6. Optional fair-value sources.** Add Kalshi rows to `mapping.csv`, and
rating-based rows to `fair.csv`.

**7. Dry run, then live.**
```bash
./sigcup -mode dry      # read the would_rest / would_trade lines for an hour
./sigcup -mode live     # or: install sigcup.service and start it
```

## Things to know once it's live

- **Editing `targets.csv` takes effect within a minute, with no restart.**
  Rerun `./run_sim.sh` whenever prices or your view change. Deleting the
  file turns accumulation off.
- **Don't place manual orders on stack contracts while the bot runs.** It
  treats any resting order on a stack contract, in the stack's direction,
  as its own, and will reprice or cancel it.
- **The daily-loss halt covers the stack too.** A bad poll can knock a big
  directional stack down 10% in a day. The halt then pulls your bids and
  stops buying exactly when prices are cheapest. Once the stack is large,
  consider `-daily-loss 0.2`.
- **Fair value never trades against the stack.** It skips any trade that
  would sell what the stack is buying. The digest counts these.
- **The sim doesn't model** election-night rolling of the reserve, what
  other players hold, or chamber control built seat by seat. Control
  markets ride the national swing instead.
