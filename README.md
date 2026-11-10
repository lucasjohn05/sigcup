# sigcup

Standalone bot and simulator for the SIG Predictions Cup.

**Start with `YOU_FILL_IN.md`.** It lists every input that's yours, in
order.

- `sim` prices all 117 races off one national swing and finds the stack
  that maximizes P(final balance >= the score you think wins), not
  expected value. It stress-tests that stack against being wrong and
  writes `targets.csv`.
- `sigcup` builds that stack patiently with resting bids, runs arb and
  fair-value trades on the side, measures depth and lag for the endgame,
  and fires pre-built election-night batches.

The results feed isn't built yet.

## The stack (sim + accumulate)

```bash
./sim -races races.seed.csv -offline -shift 2 -target 2   # explore today, no keys needed
./sim -build markets_dump.csv                              # map the live board -> races.csv
./run_sim.sh                                               # your numbers -> targets.csv
```

How the model works: each race's Dem-side margin is its market baseline
plus a shared national swing (fat-tailed), a shared state term, and local
noise. With `-shift 0` it reproduces every market price exactly; a test
enforces this. Your shift moves the national term, which is what moves races
together. The search maximizes a smoothed P(balance >= target). It starts
from several portfolios, uses add/remove/swap moves, and is judged on fresh
simulations it never saw.

`targets.csv` rows are `exchange_id,buy,max_price,target_shares,take_below,note`,
with prices in the space of the side you buy. Each accumulate cycle (once
a minute by default), the bot:

- joins or improves the best bid on your side by one tick, never above
  `max_price` and never crossing the ask;
- keeps its bid if it's already the best, and steps up only when outbid;
- caps each resting order at `-acc-chunk` SUSQies and expires it after
  `-acc-ttl`;
- takes the ask only when it's at or below a row's `take_below`;
- stops at `target_shares` and pulls its bids.

Edits to `targets.csv` load live. Fair-value trades never go against the
stack, and `flatten` never exits it.

## What it does

Every 20 seconds it:

1. Pulls your balance, positions, and top of book for every contract it
   cares about (one bulk call).
2. **Fair-value taker.** For contracts you mapped to Kalshi, it takes SUSQ
   quotes that are 5+ points through Kalshi's mid. It skips Kalshi books
   wider than 3 points and Kalshi markets that aren't open.
3. **Relationship arbitrage.** It reads SIG's relationship graph (mutually
   exclusive, complementary, implication) and Dutch-books linked contracts
   when executable prices guarantee 2+ points per set, via multi-leg.
4. Re-checks each leg against the live book, sizes it under the risk caps,
   and places a 15-second limit at the touch. Any unfilled remainder is
   cancelled and the real fill count is read back from the order's fills.

## What it handles without you

- **Uneven arb fills.** Multi-leg placement is atomic, but fills aren't. When
  legs fill unevenly, the bot first tries to complete the short legs,
  spending at most the arb's edge, so the set is no worse than break-even.
  If that fails, it flattens the excess back to the smallest leg, accepting
  up to `-unwind-slack` (3 pts) per share. It pages you only if both fail.
- **Mappings that look wrong.** If an unverified mapping shows SUSQ 15+
  points from Kalshi, the bot quarantines it and pages you once. A gap that
  large is usually an inverted row or mismatched resolution rules. If you
  check both Info tabs and the gap is real, set `verified=true` on that row
  and restart. Quarantine is sticky until restart. It won't catch an
  inverted contract trading near 50%, so still read the Info tabs.
- **Errors a retry can't fix.** `TERMS_NOT_ACKNOWLEDGED`, revoked or expired
  keys, missing scopes, and bans halt trading and page you once.
- **Resting orders.** Cancel-all runs on startup, on shutdown, and whenever
  trading gets blocked (STOP file, daily-loss halt, fatal error). Every
  order also carries an `expirationDate`.

## What reaches your phone

Pages (high priority): the bot starts or stops, a daily-loss halt, a fatal
API error, a quarantined mapping, an arb stuck unhedged, 5 consecutive tick
errors (and recovery), a failed cancel-all.

Everything else goes into one low-priority **daily digest** at
`-digest-hour` UTC (default 12, which is 8am EDT). It covers value, rank,
Smart Score, fills, arb repairs, quarantined mappings, and newly listed
markets. New listings are diffed against `known_markets.txt`, so you don't
have to check the site for markets to map.

The heartbeat covers the case where the bot dies silently. Set
`HEARTBEAT_URL` to a healthchecks.io check with a period of about 5 minutes.
When the pings stop, you get an alert.

## What it measures for the endgame

Three files fill up while the bot runs. After the first few days, they tell
you whether the election-night plan works and how much size it can take.

- **Lag** (digest line, and `lag` events in the journal). When a mapped
  Kalshi mid moves 3+ pts, the bot times how long SUSQ takes to move half as
  far. "Within one tick" means under 20s. It can't tell SIG's market maker
  from other traders, and it doesn't need to: what matters is how stale the
  book gets.
  - Mostly within one tick: stale prices on election night will only
    survive in thin markets with no liquid Kalshi counterpart. Aim the
    endgame at House seats.
  - Minutes to hours, or never: stale ladders are the prize.
- **`depth.csv`.** The bot snapshots 3 order books per tick, round robin
  over every open contract, so the whole tournament is covered about once
  an hour. Each row records size and cost to take within 5 pts of the
  touch. This is your capacity. If the median book holds 2k within 5 pts,
  you can't put 100k into one stale race.
- **`leaderboard.csv`.** Hourly top-100 snapshots. Match leaders' PnL
  changes against price moves to infer what they hold. You'll want that
  history on the last morning.

The digest also shows ALL collateral components and their guaranteed
payout floor. If arb sets show up with a floor near their cost, arb
capital is being freed and arb is close to free to run.

## Endgame tools

The fair-value taker stops opening positions at `-fv-stop` (default Nov 1,
00:00 ET). Arb keeps running. Then:

1. **Flatten October inventory (Nov 1–3).**
   `./sigcup -mode flatten` writes `flatten.csv`, an exit order for every
   open position priced 2 pts through the touch. YES holdings sell, and NO
   holdings buy back (positions net). Anything that looks like an arb set
   is written commented out, since selling one leg unhedges the rest. Edit
   the file, then stage and fire it.
2. **Pre-build batches.** Make one CSV per poll-closing wave and swing
   bucket; see `batch.example.csv`. Each row is an exchange, buy or sell
   (YES space), a worst acceptable price, and a quantity or SUSQies to
   spend.
3. **Stage.** `./sigcup -mode stage -batch batches/w2_D6.csv` shows what
   would fill right now at your limits and what it costs, without sending
   anything. Run it on the evening of Nov 3 to check the books still look
   the way you planned.
4. **Fire.** `./sigcup -mode fire -batch batches/w2_D6.csv -confirm` sends
   the whole batch via `/orders/batch`, cancels remainders after
   `-fire-ttl` (10s), and prints what filled. Without `-confirm`, it only
   stages. Fire bypasses the October risk caps on purpose, but it still
   respects the STOP file.

Fire and the live bot share the same account. Put a STOP file down or stop
the service before firing, so the bot's arb doesn't compete with you for
cash.

## Reading the digest for competition

Three numbers show whether other bots are working the same edges:

- **gone before execution**: the edge disappeared between the price scan
  and the book re-check.
- **lost the race**: the bot placed an order and nothing filled.
- **avg SUSQ-Kalshi gap**: how closely SUSQ tracks Kalshi on mapped
  contracts.

If the first two are high and the gap is about 1 pt, someone is polling
faster than you. Try `-interval 5s` and watch the journal for `429`s. If
the gap stays wide and fills are easy, the field is mostly manual.

## Rules check (manual; the API has no rules text)

`-mode markets` prints every market that settles after the tournament ends.
Read those Info tabs first, then any race where:

- an independent could win, so Dem and GOP can both resolve NO. Don't treat
  that pair as complementary unless SIG's relationship graph says so;
- a runoff (Georgia, Louisiana) or ranked-choice count (Maine, Alaska) runs
  past Nov 4;
- a recount is plausible.

The question for each is what SIG does if the race is undecided when the
tournament ends. Bots mapped to Kalshi and students pricing off headlines
will miss this.

## Setup

```bash
cp .env.example .env        # paste key, slug, ALERT_URL, HEARTBEAT_URL
go build -o sigcup ./cmd/sigcup
./sigcup -mode tournaments  # find the tournament slug
./sigcup -mode markets      # writes markets_dump.csv (every contract + id)
```

Accept the competition terms in the web UI first.

Build `mapping.csv` from `markets_dump.csv`: one line per SUSQ contract you
want priced off Kalshi. Only map contracts whose resolution rules match, and
set `invert=true` when the SUSQ YES is Kalshi's NO. Arbitrage needs no
mapping.

## Modes

| Mode | What it does |
|---|---|
| `scan` | one pass, logs what it would trade, exits |
| `dry` | loops, logs `would_trade`, never places orders |
| `live` | loops and trades |
| `stage` | shows what a batch would fill right now; sends nothing |
| `fire` | sends a batch (needs `-confirm`) |
| `flatten` | writes open positions as an exit batch |

Run `dry` for a day after Oct 1, read the `would_trade` lines, then switch to
`live`.

## Running it

Put it on an always-on box (any $5/month VPS) under systemd; see
`sigcup.service`. Kill switch: `touch STOP` in the working directory. The
bot cancels resting orders and blocks new ones until you delete the file.
New orders also stop if value drops 10% within a UTC day (resets at
midnight).

## Defaults (all flags)

| Flag | Default | What it does |
|---|---|---|
| `-edge` | 0.05 | min points through Kalshi mid |
| `-kalshi-max-spread` | 0.03 | ignore wider Kalshi books |
| `-arb-edge` | 0.02 | min guaranteed profit per arb set |
| `-max-order` | 2000 | SUSQies per order |
| `-max-exchange` | 5000 | cost basis per contract |
| `-max-deployed` | 0.4 | share of value in positions; keeps 60% liquid for the endgame |
| `-daily-loss` | 0.10 | daily-loss halt |
| `-min-price` / `-max-price` | 0.03 / 0.97 | never take outside this band |
| `-suspect-gap` | 0.15 | quarantine threshold, 0 = off |
| `-repair-ttl` | 5s | expiry for repair/unwind orders |
| `-unwind-slack` | 0.03 | max per-share loss to flatten a stuck arb leg |
| `-digest-hour` | 12 | UTC hour for the digest, -1 = off |
| `-fv-stop` | 2026-11-01T04:00:00Z | no new fair-value trades after this; `""` = never |
| `-lag-thresh` | 0.03 | Kalshi move that starts a lag measurement, 0 = off |
| `-lag-max-wait` | 6h | lag events still open after this count as never followed |
| `-depth-per-tick` | 3 | books snapshotted per tick, 0 = off |
| `-fire-ttl` | 10s | fire: order expiry before remainders are cancelled |
| `-flatten-slack` | 0.02 | flatten: how far through the touch to price exits |
| `-targets` | targets.csv | stack file from `sim`; missing = accumulation off |
| `-acc-chunk` | 1000 | max SUSQies per resting stack order |
| `-acc-ttl` | 30m | resting stack orders expire after this |
| `-acc-every` | 3 | work the stack every N ticks |
| `-acc-max` | 0.8 | stack at max prices capped at this share of value (scaled down, with an alert) |
| `-fair` / `-fair-edge` | fair.csv / 0.08 | your fair values for contracts with no Kalshi market |

These defaults are tighter than the original. Unattended, you want fewer,
cleaner trades.

## Verify on day 1 (the spec doesn't say)

- Order book prices are assumed to be in YES space. Confirm on one contract.
- Relationship nodes are assumed to reference the YES outcome. The bot skips
  any node that doesn't.
- Positions come back as one signed quantity per contract (YES positive, NO
  negative), so YES and NO should net. The arb unwind and `flatten` rely on
  this. Confirm it once with a tiny sell-then-buy by hand.
- Rate limits and position limits aren't documented. Watch for `429`s.
- Place one tiny manual order through the bot before trusting sizes.

## Not built yet

- Election-night results feed and swing model (the batches are built by
  hand until then)
- Realtime websocket
- Resting market-making quotes (deliberately left out; they need watching)
