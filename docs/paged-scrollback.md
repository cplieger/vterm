# Design: demand-paged scrollback

Status: RATIFIED (owner decisions dated inline; adversarially reviewed, see
§11). Targets the engine minor after v3.7.0.

## 1. Problem

Today the client store is all-resident: every scrollback line the client may
ever show must be held in its `LineStore` (and rendered into DOM) from the
moment it arrives. The cap (`maxLines`, kiro passes 5000) is simultaneously:

- the memory budget — the dominant per-tab allocation implicated in iOS
  jetsam kills (multi-MB store + tens of MB of row DOM + an ~85,000 CSS px
  scrolled-contents compositor layer at 5000 rows), and
- the history ceiling — scrolling past it is impossible even though the
  server ring (`WithScrollbackCapacity`, kiro passes 5000) still holds the
  lines. Users hit this ceiling and find it frustrating.

Lowering the cap fixes memory and worsens the ceiling; raising it does the
reverse. The two concerns are coupled only because history residency is
eager. This design decouples them: a small resident tail plus on-demand
paging of older history from the server ring, which is and remains the
source of truth.

Ancillary win: the `scrollbackLines` consumer knob (unreleased, ui 5.3.0)
stops being a user-facing depth tradeoff and becomes an internal residency
tuning value. No consumer needs to think about it anymore.

## 2. Prior art

Surveyed 2026-08; links inline. Content rephrased for compliance with
licensing restrictions.

<!-- markdownlint-disable MD013 -->

| System | Model | Numbers |
| --- | --- | --- |
| [kitty](https://sw.kovidgoyal.net/kitty/conf/) | Two tiers: a small in-RAM interactive buffer + a separate large history store accessed on demand (via pager). The deep tier ships OPT-IN (default 0); the docs explicitly steer users toward it and away from inflating the resident buffer | `scrollback_lines 2000` resident; `scrollback_pager_history_size` up to 4 GB, ~10k lines/MB |
| [tmux](https://man7.org/linux/man-pages/man1/tmux.1.html) | Server-held ring per pane; the attached client is a thin viewer; oldest lines discarded at the limit | `history-limit 2000` default (tmux(1)) |
| VS Code / xterm.js | All-resident client buffer ([scrollback defaults to 1000](https://xtermjs.org/docs/api/terminal/interfaces/iterminaloptions/)); a 160-col 5000-line buffer measured ~34 MB before typed-array optimization ([xterm.js #791](https://github.com/xtermjs/xterm.js/issues/791)) | On reconnect to a persistent session it restores only [`persistentSessionScrollback`](https://code.visualstudio.com/docs/terminal/advanced) lines (default 100) |
| [iTerm2](https://iterm2.com/documentation-preferences-profiles-terminal.html) | All-resident; default 1000; "unlimited" documented as possibly consuming all available memory | The cautionary pole |
| Chat/log UIs ([Slack pagination](https://api.slack.com/docs/pagination)) | Cursor-paged history fetch on scroll; standard infinite-scroll practice prefetches via an [IntersectionObserver rootMargin](https://developer.mozilla.org/en-US/docs/Web/API/IntersectionObserver/rootMargin) before the edge is reached. Slack also settles the short-page question: a shorter-than-requested page is NEVER a terminator; the terminator is a separate explicit signal | Precedent for cursor-shaped paging (our absolute indices are the cursor), NOT for page sizing — Slack recommends ≤200/page |

<!-- markdownlint-enable MD013 -->

Takeaways applied here:

1. kitty is the architectural precedent: a deliberately small interactive
   tier backed by a much larger on-demand tier (which kitty offers opt-in;
   we ship it on). We have the second tier already — the server ring — and
   it is authoritative for resume anyway.
2. tmux/VS Code ship 1000–2000 resident lines and almost nobody notices;
   VS Code restores just 100 lines on reconnect. Our current
   everything-resident 5000 is the outlier, not the baseline.
3. Chat apps solved scroll-triggered paging long ago: fetch on approach
   (not on arrival at the edge), one in-flight request, cursor = position,
   short page ≠ end of history.

## 3. Chosen values

Owner decision (2026-08): kitty's numbers are the baseline; each is then
adjusted for what makes us different — the client is a browser (a resident
line costs DOM nodes and compositor tiles, not just bytes) and the binding
constraint is a phone (iOS jetsam), not desktop RAM. Byte figures in this
table are planning estimates from the audit's arithmetic (row shapes:
plain shell output, 1–3 runs/row), not device measurements; §7's E2E row
includes the on-device check.

<!-- markdownlint-disable MD013 -->

| Constant | Value | kitty baseline → our tweak |
| --- | --- | --- |
| `residentTailCap` | 1500 | kitty ships `scrollback_lines 2000` resident. A web row costs an estimated 10–100x kitty's per-line RAM (DOM nodes + an ~17 px slice of a compositor layer per row), and jetsam kills at device-wide pressure — so tweak DOWN: 1500 ≈ kitty's default minus a mobile tax, still ~40 phone screenfuls (37 rows) and one third of today's 5000 resident lines in the never-scrolled case. Overridable via the existing `scrollbackLines` option, whose semantics narrow to exactly this (a SUPPLIED value pins both capability states to it — §5.3). |
| deep tier (server ring) | 100000 lines (the ENGINE default) | kitty's deep tier is `scrollback_pager_history_size`, sized in MB (~10k lines/MB, max 4 GB), shipped opt-in and recommended by its own docs over a bigger resident buffer. Ours is the server ring (`WithScrollbackCapacity`), whose depth costs the phone nothing at attach beyond the BOUNDED replay (≤ `replayMax` lines however deep the ring — §4.5). The owner ratified 100000 as the ENGINE's own default rather than a per-app number (superseding the 20000-set-by-kiro this section first specified): one depth for every app on the engine, no copy to drift, and a deliberately absurd number is how this family spells "never truncate" since there is no unlimited sentinel. Worst case ~10–30 MB server RAM per session at full depth (estimate), but the ring GROWS as history is produced instead of preallocating, so a session pays only for what it actually wrote and a short one costs nothing. Paging is DECLARED only at `scrollbackCapacity ≥ paginationMinRing` (`maxReplayLines + 1`, §4.5): below that the bounded replay carries the whole ring, so nothing is withheld and there is nothing to page for. |
| `pageSize` | 1000 lines, further bounded by `pageByteBudget` | kitty has no paging — it pipes the whole deep buffer to the pager at once, which is exactly wrong on LTE. Tweak: page in wire-frame units; `maxScrollLinesPerFrame` is already 1000, so one page = one standard scroll frame (~100 KB typical for plain output; styled rows can exceed it, hence the byte budget — the reply serves fewer lines rather than a bigger frame). |
| `pageByteBudget` | 256 KiB encoded | Hard per-reply bound (browser WebSockets expose no receive backpressure, so the frame IS the memory commitment). The server stops adding whole lines before the budget. Paired with a per-ROW ceiling that makes the bound real (§4.2): a row whose encoding exceeds the budget is re-encoded with hyperlink URIs stripped (text + styling preserved) — text-and-style alone is arithmetically bounded (~22 KB at the 1000-col grid max), so every reply fits and an oversized row can never masquerade as the empty "trimmed" terminator. The row ceiling applies to EVERY row the shared encoder emits (page, live flush, resume replay, screen), bounding each ROW everywhere; the aggregate live/replay MESSAGE shapes are a separate, pre-existing exposure §4.2 names rather than closes. |
| `prefetchThreshold` | 500 | No kitty analog (native scrolling is instant; ours hides a network RTT). Chat-app "fetch on approach" sized for touch: a full-speed phone flick (~3000 px/s ÷ 17 px/line ≈ 175 lines/s) gives ~3 s of headroom. |
| `browseCacheCap` | 2500 | No direct kitty analog, but the same philosophy as its pager copy: transient by design, discarded when done. Paged-in lines are a disposable cache capped at 2500 lines, so worst-case residency 1500 + 2500 = 4000 stays below today's 5000 even mid-browse. Eviction takes rows from whichever END of the cache is farther from the reader (§5.3). |
| `historyBurst` / `historyRefillEvery` | 4 / client 2 s, server 1.5 s | Per-socket token bucket on `history` controls, BOTH sides: the client paces itself (including byte-budget continuations), the server enforces a floor against abuse (requests are cheap for the client and expensive for the server: ring snapshot under `h.mu` + up to 256 KiB encode + write). The server refills FASTER than the client on purpose — independent clocks and latency jitter compress arrival spacing, so identical constants would let a healthy client trip the server's silent drop; the slack absorbs it (§4.4). Sustained 30 pages/min client-side. |
| capability signal | 1 bit in the resumeAck tail, zero requests | Capability is DECLARED, not probed: ackFlags bit1 (`historyPaging`) in the resumeAck's existing length-gated tail (§4.5). The ack is the first frame of every resume batch — always ahead of its replay — so capability is known one RTT after every acked attach, before any replay byte arrives, with no probe request, no probe timeout, no retry machinery, and no way to mis-read a slow link as an old server. The bit also declares ring DEPTH: a server sets it only when `scrollbackCapacity ≥ paginationMinRing` (`maxReplayLines + 1` — §4.5), so a ring the replay cannot truncate reads as `unsupported` and the compatibility cap holds. An old server (short tail or unset bit) reads the same way: nothing is ever sent to it. The only request timer is the data timeout (8 s) on real page fetches. |

<!-- markdownlint-enable MD013 -->

Memory picture at these numbers, kiro on a phone: common case 1500
resident — the cap flips one RTT after attach on the resumeAck bit, and a
cold attach downloads ≤ `replayMax` lines however deep the ring (§4.5).
Two shapes hold more, both transient and both TTL-bounded: a FOLLOWING
viewport (the common case — a following viewport counts as outside every
reclassified band, §5.3) drains a reclassified band to
`prefetchThreshold`, so the first attach over a legacy 5000-line
snapshot holds ~2000 and the ordinary stale reconnect after this design
ships holds ~2000 as well (a 1500-line store reclassified, drained to
500, plus the ~1460-line replay); a viewport PARKED on reclassified rows
keeps `browseCacheCap` instead, up to 4000 — inside the deep-browse
bound below — until the reader moves on or the 5-minute TTL fires.
Deep-browse steady worst
case 4000, with a transient store-line peak of 5000 for the window
between a full page apply and its own budget pass (§5.3 — equal to
today's cap, held for microseconds, never sustained). History reachable
by scrolling: the ring's depth, 100000 lines by default (was 5000, and it
was a hard wall).

## 4. Wire protocol

One new client→server control on the existing JSON control channel. No new
server→client message type and no request id: replies are ordinary scroll
frames made correlatable by the rules in §4.3. Correlation mistakes are
CONTENT-SAFE by construction — within a boot epoch a COMMITTED absolute
index (below the live window) is immutable, and every range a page
request can target is committed by construction (strictly below the live
tail's oldest line; window rows at their indices mutate legitimately via
`applyScreen` until they commit, which is why the proof is scoped) — so
any frame that happens to satisfy the correlation window carries the same
bytes the awaited reply would have carried. Correlation
exists to drive client STATE, and the residual risk is bounded rather than
zero: a mis-correlated frame that reads as a CLAMP raises `pagingFloor`
falsely, and its repair is §4.5's resumeAck lowering rule (that rule's main
job). What makes mis-correlation hard in the first place is an invariant
worth stating: the live tail's low edge is monotonically non-decreasing
within a socket (trims only raise it; a page apply is classified browse
even when flush against the tail, §5.2), so a correlation window computed
below the tail at send time stays below the tail at reply time, and the
only frames carrying sub-tail rows are page replies. As hardening, a clamp
is accepted only when its `firstIndex` is at or above the last
resume-reported `serverOldest` — every honest clamp satisfies that.

### 4.1 Request

```json
{ "type": "history", "fromAbs": 3000, "maxLines": 1000 }
```

Validation (server, before any work): `maxLines` an integer in
`[1, pageSize]` FIRST, then `fromAbs` an integer with
`0 ≤ fromAbs ≤ (2^53 − 1) − maxLines` — the subtraction form, because the
addition form (`fromAbs + maxLines ≤ 2^53 − 1`) is itself the int64
overflow it exists to reject. The exclusive end `end = fromAbs + maxLines`
is then exact in both languages. Decoded into signed fields and validated
BEFORE conversion to `uint64`; anything else is dropped with a Debug log.
A missing `fromAbs` decodes to 0 and is accepted as a deliberate request
at the epoch origin (the intersection clamps it; stated so nobody adds a
pointer field for absence). The client enforces the same domain before
sending (`Number.isSafeInteger` on `fromAbs`, `maxLines`, and their sum),
and the DECODER side rejects unsafe inbound indices (`firstIndex`,
`firstIndex + numLines`, `base + height`, resume bounds) rather than
letting `Number()` round them — cross-language exactness is end to end or
it is nothing. A `history` control is ignored until the socket's first
successful resume — the ATTACHED SESSION (the reply's ack source,
`clientState.session`) does not exist before one, and the ignore keeps
pre-resume sockets cost-free. The client orders this by construction: its
bootstrap sends resume before anything else on a fresh socket, and
controls are FIFO. Never sent as a pre-upgrade text frame (text controls
before the v4 latch close the socket; the client only uses text controls
once upgraded — existing invariant, restated because `history` is the
first control added since it was established).

### 4.2 Serving

Served INLINE on the socket's read loop, like every other control: snapshot
under `h.mu`, then write under the socket's `writeMu` (so a reply can never
interleave into a resume batch — the §7 ordering test is a regression
guard, not a race repro). The handler's arithmetic is normative, guards
before subtractions (uint64):

```text
end   := fromAbs + maxLines                 // exact; bounded by §4.1
start := max(fromAbs, ring.OldestIndex())
lim   := min(end, ring.Committed())
if start >= lim:
    reply empty: encodeScrollMsg(ack, firstIndex = fromAbs, nil); return
n     := lim - start                        // 1..maxLines
lines := ring.LinesRange(start, n)          // NEW bounded accessor,
                                            // sibling of LinesFrom
n     =  shrinkToBudget(lines, pageByteBudget)  // whole lines, >= 1; keeps
                                            // the OLDEST lines (a prefix)
reply := encodeScrollMsg(ack, firstIndex = start, lines[:n])
```

The serve is the INTERSECTION of the request window and the retained
range — never lines the client did not ask for — so every non-empty
reply's `firstIndex` lies inside `[fromAbs, end)` and correlation (§4.3)
always succeeds. (`LinesRange(abs, max)` is a ~10-line sibling of
`LinesFrom` with a count bound; it exists because `LinesFrom` returns
every line from the clamp to the tail — an O(ring) copy per request on a
20000-line ring.)

`shrinkToBudget` keeps a PREFIX (the oldest lines), and that direction is
FORCED, not stylistic: shrinking from the low end would move `firstIndex`
above `fromAbs`, which §4.3 defines as the CLAMP signal — every styled
page would then raise `pagingFloor` and paint a false permanent "trimmed"
marker. The clamp encoding and the shrink direction are coupled; change
neither alone.

The byte accounting is MESSAGE-inclusive and normative. `encodeScrollMsg`
prepends a 19-byte header (1 type + 8 ack + 8 firstIndex + 2 numLines);
one `encodedRowSize(row)` helper (2-byte run count plus, per run, 18
fixed bytes + text + URI) drives packing, stripping, and the tests, so
the three cannot drift. `shrinkToBudget` packs whole rows while
`19 + Σ encodedRowSize ≤ pageByteBudget`; the ROW CEILING strips any
single row with `encodedRowSize > pageByteBudget − 19`, so the largest
legal unstripped row is 262,125 bytes (the 262,125/262,126 boundary pair
is a §7 test) and a one-row reply always fits — `numLines == 0` keeps
exactly one meaning. Stripping re-encodes the row with hyperlink URIs
emptied AND the autolink attribute bit (1024) cleared, so a stripped run
is indistinguishable from a never-linked one (no `A & 1024`-with-empty-`U`
state the wire has never carried). The strip decision is a PURE FUNCTION
of the canonical committed row — never of remaining budget or delivery
path — so repeated delivery of one index is byte-identical on every path
and idempotence holds. Text-plus-style is arithmetically bounded on two
LOAD-BEARING premises, both stated: `cols ≤ maxResizeCols` (1000,
enforced by `clampResize`), and one rune per cell at ≤ 4 UTF-8 bytes
(combining marks are consumed at width 0 and never stored; runs are
style-coalesced, never split below a cell) — 18 bytes of per-run overhead
gives ≈ 22 KB worst case. Raise `maxResizeCols` or store grapheme
clusters per cell and the ceiling scales or dissolves; §7 pins the test
arithmetic to the constant, not the literal.

The ceiling lives in `appendRowRuns`, the row encoder shared by ALL
row-emitting paths — page, live flush, resume replay, and SCREEN frames —
deliberately including screen so a pathological row displays and pages
identically (link-less in both) instead of showing links on screen and
losing them the moment it scrolls off. What a stripped row loses is named
honestly: the client re-linkifies from VISIBLE text (`linkifySpans`
re-scans plain spans, per span), so a stripped server-stamped autolink
whose URL is split by a style change or a soft wrap re-derives a PREFIX
href, and an OSC 8 link whose text is not a URL ("click here", a
filename) loses its link silently — accepted degradation, reachable only
above the 256 KiB row threshold. And the scope is bounded honestly: the
per-row ceiling bounds every ROW everywhere; it does NOT bound the
aggregate live/replay/SCREEN messages: live and replay group up to
1000 / 50 rows per scroll message (`maxScrollLinesPerFrame`, resume
chunking), and a full screen frame carries up to 1000 changed rows
(27-byte header plus a 2-byte row index each) — worst legal shapes
post-ceiling ≈ 250 MB live, ≈ 250 MB screen, ≈ 13 MB replay chunk (from
~4.1 GB / ~4.1 GB / ~205 MB before the ceiling, ~16x better), all
requiring an adversarial app emitting maximal styled rows. Byte-aware
chunking of the frame model is deliberately
out of scope; named here so the row ceiling cannot read as closing it.

The write uses a 10 s context — the resume-batch precedent, not the 5 s
live-dispatch one, and deliberately no larger, because the blast radius
of a slow reply write is the SESSION, not the socket: the reply holds the
socket's `writeMu`, and `dispatchFrame`'s fan-out `wg.Wait()`s on every
client's write (frames are not regenerable — a skipped frame would be a
permanent scrollback hole), so while one client's page reply crawls, live
output to EVERY client of the session stalls, and the serving socket's
own read loop — the goroutine that pumps its PTY input, including an
interrupting Ctrl-C — is occupied for the same window. This is the
pre-existing wedged-client class (5 s live dispatch, 10 s resume batch)
entered from a new, routine path; 10 s matches the worst constant already
accepted rather than extending it. The link floor this sets for a
budget-saturated reply (256 KiB / 10 s ≈ 26 KB/s) is of the same order
as — and for typical content ABOVE — the bar the live path already
imposes (a ~100 KB plain-output flush frame in its 5 s dispatch context
≈ 20 KB/s), so the honest statement is parity, not shelter: a link near
either floor loses sockets on whichever path saturates first. What keeps
paging usable there is ADAPTATION, which the live path lacks: a
per-socket adaptive budget `effMax` and a remembered ceiling
`budgetCeiling` (RFC 5681's `ssthresh` shape, and the reason the
mechanism is NOT called AIMD — the ceiling is a growth TARGET, not a cap
below the current value). `budgetCeiling` starts at `pageSize`. On a data
timeout of a request sized `T`: `budgetCeiling = max(125, ⌊T / 2⌋)` and
`effMax` drops to the 125 FLOOR. On each CONTAINED correlated reply
(§4.3): `effMax = min(2 · effMax, budgetCeiling)`. So a link that
carries 500 but not 1000 times out ONCE, climbs 125 → 250 → 500 over its
next three replies, and stays at 500 for the socket's life; a link that
then degrades further re-halves the ceiling and re-converges from the
floor, always approaching the largest size that actually works FROM
BELOW rather than pinning below it. `effMax`, `budgetCeiling`, and the
remembered `T` live in the concrete socket's closure and reset with it
(§4.4's atomic list — a reconnect is usually a link change); the
trigger reads `effMax` at fire time through a consumer-wired
`historyBudget()` accessor beside `requestHistory` (§5.4 — the
controller lives in `render.ts` and has no other channel to
connection-owned state), and it feeds BOTH the trigger's anchor and its
length, so a shrunken retry stays adjacent to the reader's edge instead
of healing the far end of the gap. The byte budget still bounds every
reply independently, and the single-row worst case (one 262,125-byte
row) cannot shrink and keeps the 26 KB/s floor (accepted; it needs a
pathological row). The structural fix — a
per-socket bounded outbound queue with one writer goroutine, closing the
whole wedged-client class — is out of scope here, noted for its own
design. Ordering is normative: the CLIENT acts first — its 8 s data
timeout (§4.5) releases single-flight and retries on a still-live socket,
which is the whole point of the data-timeout path. In coder/websocket a
write-context expiry CLOSES the connection, so a server deadline below
the client's would convert every slow reply into a disconnect +
reconnect, exactly the failure the separate data timeout
exists to avoid; the chain is data 8 s < write 10 s. A reply still
crawling after the client gave up either completes (landing as a
dropped-by-guard-2 stray) or fails at 10 s and closes the socket; both
are clean. On a timeout-prone link the steady state is up to ~2x bytes
per page (the timed-out reply usually completes, lands inside the
shrunken retry's window, and applies its in-window lines; the retry's
own duplicate is then dropped) — accepted, and
progress is still made each cycle; the halving rule bounds what each
further cycle can waste. The reply is stamped with the socket's current
ack value but does NOT update `lastAckSent` (the next ack-only sweep may
send one redundant ack; harmless, the client's apply is monotonic).

### 4.3 Reply semantics (normative)

- A non-empty reply carries `firstIndex = start ∈ [fromAbs, end)` and
  `1..maxLines` lines, never crossing `end`. `firstIndex > fromAbs` is the
  CLAMP signal: `[fromAbs, firstIndex)` is evicted server-side, forever —
  raise `pagingFloor` (§4.5) to `firstIndex` and render the permanent
  trim marker at that edge.
- An EMPTY reply (`numLines == 0`) carries `firstIndex = fromAbs` (the
  handler encodes the request's own start — never `LinesFrom`'s
  empty-case `firstAbs`, which is `committed`, an index far outside the
  window). It means nothing in `[fromAbs, end)` is retained: raise
  `pagingFloor` to `end` and mark permanently. The raise is sound because
  a request can never reach above `committed`: the trigger caps
  `maxLines` at the absent run's length, the highest reachable edge is
  the tail's low edge, and the tail's low edge is at most `committed`
  (the window base equals it) — so an empty reply always means "trimmed",
  never "not yet written". For the frontier this is exact ("nothing below
  the frontier survives"); for an interior gap the raise condemns only
  `[fromAbs, end)` — the gap's remainder above `end` stays fetchable. The
  scalar floor is sound because ring retention is a suffix ("nothing at
  or below x" is global, not per-gap).
- A reply may serve FEWER lines than requested (`pageByteBudget`). A short
  page is NEVER a terminator (the Slack rule, §2): the controller's next
  paced trigger requests the remainder. Only a clamp or an empty reply
  terminates, and both only raise `pagingFloor` — they never disable
  paging.
- Correlation: a scroll frame is the page reply iff its `firstIndex` lies
  in `[fromAbs, end)` of the in-flight request. CONTAINMENT gates the
  control effects: single-flight release and `effMax` growth belong only
  to a reply CONTAINED in the window
  (`firstIndex + numLines ≤ end`) — a correlated frame that extends
  BEYOND the window (a timed-out larger reply sharing the retry's
  `fromAbs`, possible on the lower-edge approach where halving shrinks
  only `end`) applies its in-window intersection (§5.1) and changes NO
  control state; the attempt's own reply, when it arrives, releases and
  grows as usual, and if the stale frame already healed the window the
  contained duplicate applies idempotently. Range-disjointness makes a
  false match unlikely — live flush frames carry `firstIndex` at or above
  the client's previous window BASE (they are the rows that just scrolled
  off it), resume replay chunks carry `firstIndex > haveThrough`, and
  every page request targets a range that was ABSENT at send time, i.e.
  strictly below the live tail's oldest line — and the content-safety
  argument (§4 intro) makes the residual harmless. A frame failing the
  test never releases single-flight and never touches paging state.

### 4.4 Rate and abuse bound

Two token buckets with deliberate asymmetry (client refills every 2 s,
server every 1.5 s, burst 4 both — the server's slack absorbs clock and
latency jitter that compresses arrival spacing, so a paced client stays
under the server's floor with margin rather than by coincidence):

- CLIENT-side pacing in `requestHistory`: at most one send per client
  refill, burst `historyBurst`; state lives in the CONCRETE socket's
  closure and resets atomically with capability, single-flight, `effMax`,
  and its remembered timeout size when
  the socket is replaced (never a module-lifetime bucket — a surviving
  empty bucket after reconnect would stall against a fresh server bucket,
  and a reset one mid-socket could burst against a depleted server; a
  reconnect is usually a link change, so a carried-over 125-line budget
  would be as wrong as a carried-over empty bucket). This
  paces byte-budget continuations too.
- A trigger denied a token is NOT merely dropped: pacing returns the
  refill instant, and the controller keeps ONE coalesced pending demand
  with a timer armed for that instant (re-running the full trigger guard
  set on fire; the guards make a spurious run free). Without the timer, a
  byte-short continuation on an idle session would stall forever — the
  reader is stationary, no scroll or flush event ever re-fires the
  trigger. The same timer serves the data-timeout retry (§4.5). Cancelled
  on socket close/replace, epoch reset, ED3, session switch, ALT
  ACTIVATION (the §5.4 trigger guard also refuses to fire in alt — the
  guard is the load-bearing half, the cancellation is hygiene; on alt
  exit the ordinary event-driven trigger takes over, no timer needs to
  survive the flip), and when the gap it was armed for heals.
- SERVER-side floor on `clientState` (same read-loop-owned pattern as the
  shipped resume throttle, §10 #8d): over-limit requests are dropped with
  a Warn. Scope stated precisely: this is per-socket FAIRNESS and
  accidental-burst suppression, NOT an aggregate abuse bound — the
  registry admits multiple sockets per session with no admission cap, so
  an authorized client opening N sockets holds N fresh buckets
  (N × 4 × 256 KiB of first-burst work) and reconnect churn renews
  credits. Accepted as pre-existing class: the resume path has the same
  per-socket shape and N-socket multiplication, and the audience is
  authenticated; a handler-level aggregate limiter is the upgrade path if
  unauthenticated exposure ever appears. Single-flight is a client
  discipline that bounds nothing server-side (controls are served
  serially on the read loop — a queued burst is one expensive page per
  request without the bucket). With the slack, a healthy paced client
  does not trip it under ordinary jitter; "cannot ever" is deliberately
  not claimed.

`maxLines` clamps to `pageSize`, the byte budget + row ceiling clamp the
reply, and pre-resume requests are ignored (§4.1).

### 4.5 Capability, the bounded replay, and the paging floor

Capability is DECLARED by the server, not probed by the client. The
resumeAck's existing length-gated tail (≥ 35 bytes:
`[serverWireVersion, ackFlags]`) gains ackFlags bit1 — `historyPaging`
(bit0 remains `ledgerLost`) — set when BOTH hold: the server serves the
`history` control AND `scrollbackCapacity ≥ paginationMinRing`
(`maxReplayLines + 1`). The depth condition is DERIVED, and must stay
derived: the resume replay is clamped unconditionally, so any ring deeper
than that clamp withholds rows from a fresh attach and paging is the only
way the client can ask for them. An independently chosen threshold (5000,
the legacy client default, against a 2000 bound) left rings of 2001..4999
replaying their newest 2000 lines with the bit CLEAR — the rest stranded in
the authoritative ring for the session's life. Below the clamp the replay
carries everything, so declining to declare costs nothing and keeps the
older promise: the bit invites the client to shrink its resident tail, and
a ring that cannot back at least the legacy
depth must not invite the flip — a default-configured server
(`scrollbackCapacity` 1000) reads as `unsupported` and nothing changes.
The ack is the first frame of every resume batch, always ahead of its
replay (an unresolved socket can receive a live frame earlier; ordering
that matters here is ack-before-replay), so capability is known one RTT
after every ACKED attach with zero requests spent. Old clients ignore the
bit; a new client against an old server sees a short tail or an unset
bit. Per-socket outcome, re-read on every (re)connect (atomically with
the pacing bucket and single-flight, §4.4):

```text
pre-ack              requests illegal anyway (§4.1: ignored until resume)
resume throttled     no ack ever arrives (§10 #8d drops over-limit
                     resumes acklessly) — the socket stays pre-ack:
                     no capability, no requests, top marker neutral
tail absent (<35 B)  unsupported — a server older than the ack tail
bit1 unset           unsupported — no paging, or a ring too shallow
                     to declare it
bit1 set             supported — the §4.5 ack transition's cap-flip
                     phase runs (§5.3), then requests are legal
```

- `unsupported` is FREE and silent: the client never sends a `history`
  control, no timer exists, and there is no way to mis-read a slow link
  as an old server — a server that sets the bit IS supported for the
  socket's life. The TOP-OF-STORE marker falls back to today's
  `hasTrimmedHistory()` predicate under `unsupported`; under `supported`
  the §5.4 predicate drives it; in the pre-ack instant it is neutral
  (the legacy predicate would falsely claim "trimmed" within seconds at
  a 1500 tail on a paging-capable session). Gap markers are NOT
  capability-gated at all (§5.4).
- Ack processing is ONE STORE TRANSITION, not four calls in a documented
  order: `connection` invokes a single consumer-built closure
  (`applyResumeAck`, §5.4's port) carrying
  `{ epochChanged, committed, serverOldest, paging, sentHaveThrough,
  sentReplayMax, viewportAbs }` — the sent values are captured by
  `connection` when it BUILDS the resume control, because prediction
  must use the inputs the SERVER saw, never store state that moved since
  (§5.2). `committed`/`serverOldest` are NULLABLE AS A PAIR (both or
  neither): the ack's bounds live in its own length-gated tail, so a
  9-byte legacy ack carries neither and a 17-byte one carries an epoch
  without them. A bounds-less ack still runs the transition — steps (1)
  and (3) are exactly what it exists to deliver (an epoch change must
  still reset; a tail-less server must still read `unsupported`) — and
  SKIPS steps (2) and (4), which have no inputs. Inventing zero bounds
  is forbidden: it would lower `pagingFloor` and forge a prediction.
  CALL SITE: the transition replaces the shipped `onResumeBounds`
  consumer callback (same position in the ack path, before the
  ledger-loss and forgotten-session returns) — that callback is
  SUBSUMED, not kept alongside, or the order splits across two
  callbacks again. One external ordering constraint: `connection`'s own
  epoch-mismatch reset (which fires the consumer's `onServerRestart`
  wholesale store reset, §5.1) must stay BEFORE the transition, as it is
  today; a consumer reset landing after would revert `effectiveTailCap`
  to `compatibilityCap` while `connection` believed capability was
  `supported`, stranding 5000 resident lines on a paging-capable server
  until the next attach. Inside the transition, in order: (1) an epoch
  change resets the store (floor cleared, cap reverted to
  `compatibilityCap`); (2) bounds apply —
  `noteResumeBounds` KEEPS `committed` alongside
  `oldest`; (3) capability applies (bit1 → the §5.3 cap flip: cap set +
  flip-band reclassify); (4) REPLAY-JUMP PREDICTION, `supported` ONLY
  (§5.2 — under `unsupported` the stranded band stays TAIL, today's
  behavior exactly: no browse reclassification, no TTL stamp, no gap
  markers of this design's making, and residency bounded by the
  compatibility cap. Scope the promise honestly: those properties hold
  while the band plus the replay fit under `compatibilityCap`. A
  DEEP-RING old server does not honor `replayMax`, so a long-absence
  resume can replay far more than the cap holds; `enforceCap` then
  evicts the band as ordinary tail, advances `everEvictedThrough`, and
  reconverges to one contiguous run with today's trim marker — which is
  precisely today's behavior, and the reason the gate's claim is
  "no worse than today" rather than "lossless". Reclassifying the band
  instead would hand disposable-cache
  semantics to history no paging can restore, a worse cut than the
  regression the compatibility cap exists to prevent):
  compute
  `replayFrom_pred = max(sentHaveThrough + 1, serverOldest,
  sentReplayMax != null ? committed − sentReplayMax : 0)` — the middle
  term covers the plain eviction gap every server produces today, the
  last term the clamp (evaluated only when the field was actually SENT,
  and sound because the sent value IS the honored value, § above) — and
  if
  `replayFrom_pred > sentHaveThrough + 1`, reclassify the stranded band
  `[oldestHeld, sentHaveThrough]` (on an empty or fresh store the
  predicate fires vacuously and the reclassify is a NO-OP — not a
  detected jump; wire nothing to it); (5) ONE viewport-aware budget pass
  (§5.3) if steps (3)–(4) reclassified anything, over the WIDER of the
  two bands (they are nested by construction: both start at
  `oldestHeld`, and the jump band's top edge is at or above the flip
  band's). The transition
  completes before ANY frame of the batch — window frame included —
  applies, and it runs BEFORE the handler's early-out branches (the
  ledger-loss and forgotten-session returns sit inside the ack path
  today; a ledger loss is not a capability event, and the long-absence
  attach that loses its ledger is exactly the one carrying a replay
  jump).
- PRE-ACK CONTENT IS SUPPRESSED, FIELD-AWARE: the server's flush can
  deliver live screen/scroll frames to a not-yet-acked socket
  (registration precedes resume, and the accept path marks the session
  dirty — so a pre-ack frame is the DESIGNED first delivery on a busy
  session, not a race), and applying their rows would mutate the store
  under a stale cap before the transition above recalibrates it: on an
  already-flipped store a single pre-ack frame can push `tailCount` over
  the cap and `enforceCap` would eat the stranded band the jump step
  exists to protect. So `connection` suppresses the ROW and WINDOW
  mutation of pre-ack screen/scroll frames — and nothing else about
  them, because a screen frame is not only rows:
  - `scrollbackCleared` (ED3) is ROUTED to the store's clear path
    (`applyScrollbackCleared`, §5.5). It is a consumed
    one-shot: the server clears its pending flag when it builds that
    frame, and the resume batch's own window frame hard-codes the flag
    FALSE, so dropping it wholesale would leave the client showing
    history the server has discarded — the one pre-ack bit that is
    genuinely unrecoverable.
  - the piggybacked `inputAck` is APPLIED (monotone, and the resumeAck's
    own ack is read at resume time, so the ledger accounting is
    unaffected either way).
  - `bell` is DROPPED, accepted and named: it is a notification about a
    screen the user has not been shown yet, and the alternative is
    ringing for output that the batch is about to repaint.
  - resumeAck, ackOnly, pong, modes, title, and clipboard PASS
    unchanged (modes and title are re-sent by the batch; clipboard is
    its own message type and its own one-shot).
  Row suppression is LOSSLESS by supersession, and each link is
  load-bearing: `writeMu` serializes the batch against the flush, and a
  frame's scroll lines are appended to the ring BEFORE dispatch, so a
  suppressed frame's lines are in the ring at the resume snapshot and
  the replay from `sentHaveThrough + 1` re-delivers them; the batch's
  window frame carries the screen; and anything the clamp withheld is
  fetchable through the gap it leaves. The rule's safety rests on "an
  ack always follows", which holds for this client: the only ackless
  server paths are an empty session id and an over-limit resume, and the
  shipped client sends exactly one resume per socket against a bucket
  that starts full at burst 10 (§10 #8d) — 11 resumes on one socket is
  unreachable from it. A consumer that hand-rolled resume spam could
  suppress forever on a socket that never acks; recorded here so the
  dependency is explicit rather than re-derived.
- BOUNDED REPLAY: the client's resume control gains an OPTIONAL
  `replayMax` field, sent as
  `min(max(1, effectiveTarget − windowHeight), maxReplayLines)`
  (`effectiveTarget` = `supportedTarget` or the supplied
  `scrollbackLines`; subtracting the
  window keeps replay + window frame ≤ the cap, so a cold attach does
  not download rows `enforceCap` immediately trims — with one stated
  degenerate shape: a window TALLER than the target collapses the send
  to 1, an attach of the window plus one history line, and paging heals
  the rest on approach). `maxReplayLines` (2000 — resident-tail order,
  deliberately NOT the ring depth) is applied on BOTH sides: the client
  clamps before sending and the server clamps what it honors, so the
  SENT value and the HONORED value are the same number by construction.
  That identity is load-bearing, not tidiness — the §4.5 jump prediction
  computes `committed − sentReplayMax`, so a server that silently
  honored something smaller would place the real replay start above the
  prediction and leave a genuine jump undetected (the r7-H1n failure
  class with a server-side cause). `maxReplayLines` is therefore a
  SHARED protocol constant, and `replayMax` has exactly one meaning
  everywhere in this document: the already-clamped value. A consumer
  that supplied `scrollbackLines: 10000` backfills 2000 on attach and
  pages the rest on demand; the batch's byte-time stays
  planning-bounded (~200 KB typical at the 2000-line server maximum
  ≈ 20 KB/s over the 10 s context, the live bar). The wire contract is
  exact, in §4.1's
  discipline: ABSENT means full replay, today's behavior for every old
  client (server-side the field is decoded FIELD-LOCALLY and
  advisory-safe — `json.RawMessage` or a tolerant nullable-int type —
  because the shipped handler drops any control whose unmarshal returns
  an error, and `encoding/json` reports an `UnmarshalTypeError` for a
  malformed value even though it skips the field and completes the rest;
  tolerating that one error class for advisory fields is the alternative
  fix, riskier for the struct's other fields. `HaveThrough *int64`
  carries the identical exposure today and is left as-is: it is a
  shipped field, not one this design adds); malformed or `< 1` reads as
  absent, never a failed resume. A server
  honors the field ONLY when it declares paging (bit1): an undeclared
  pairing replays in full, exactly today, so an unsupported client never
  loses backfill to a field it sent optimistically. When honored:
  `replayFrom = max(haveThrough + 1, committed − replayMax)` (uint64
  guard: `committed < replayMax` → 0; `haveThrough = -1` means 0), so an
  attach downloads at most `replayMax` lines HOWEVER deep the ring. At
  the DEFAULT target that is ~150 KB (§3's estimate at ~1460 lines) over
  the 10 s batch context ≈ 15 KB/s; at the 2000-line server maximum
  ~200 KB ≈ 20 KB/s — both below the live bar, and the legal
  adversarial extremes stay the §4.2 aggregate
  residuals rather than being re-bounded here. The withheld band
  `[serverOldest, replayFrom)` is exactly what the ack bounds describe;
  nothing is guessed.
- COMPATIBILITY TAIL CAP: the residency rule this bit drives — the
  effective tail cap holding at the compatibility value until the ack
  declares paging, then flipping one-way via the ack transition's
  cap-flip phase — lives
  with the other residency rules in §5.3. The reason it exists is
  compatibility: pairing the new client with an OLD server must not
  silently cut reachable history 5000 → 1500 with no paging to
  compensate — a 70% regression on the exact deployment the capability
  bit exists to tolerate (and the `paginationMinRing` condition above
  extends the same promise to a NEW server whose ring the replay bound
  cannot truncate). §6's "default
  flips to 1500" means the post-ack steady state.
- `pagingFloor` (client-side): the lowest index worth requesting. OWNED BY
  `LineStore`, like every other piece of history state, so one reset path
  governs it: `reset()` (boot epoch change) clears it; ED3 snaps it to
  the cleared bound (history below an ED3 clear is genuinely
  unrecoverable server-side — the marker it produces is TRUE; that is
  the CONNECTED path — an ED3 that happened while disconnected is never
  signalled on resume, and costs one wasted fetch instead: the first
  request into the cleared region returns a clamp at `serverOldest`,
  which raises the floor with the §4 hardening rule satisfied);
  `noteResumeBounds` lowers it when a resumeAck reports
  `oldest < pagingFloor` (within an epoch the ring's oldest only rises,
  so this lowering is in practice the REPAIR PATH for a mis-correlated
  clamp, §4 intro — keep it even though it looks unreachable); clamp and
  empty replies raise it (§4.3). Reset ordering: the ack transition's
  step order above (reset before bounds before capability before jump).
- DATA TIMEOUT (8 s), per in-flight request: releases single-flight and
  `solicitedPending`, returns the gap's marker to its idle state, sets
  `budgetCeiling = max(125, ⌊timedOutSize / 2⌋)` and drops `effMax` to
  the 125 floor (§4.2's RFC 5681 shape), and arms the §4.4
  pending-demand timer for the retry. A CONTAINED correlated reply
  (§4.3) grows `effMax` by `min(2 · effMax, budgetCeiling)`, so recovery
  converges on the largest size the link carries FROM BELOW and a
  once-degraded socket is never pinned under it. The
  §5.4 request arithmetic reads `effMax` (via `historyBudget()`) in BOTH
  the anchor and the
  length (a shrunken retry stays adjacent to the reader's edge). A late
  reply (arriving after its data timeout released single-flight and
  cleared `solicitedPending`) correlates against nothing, applies
  nothing (guard 2 drops it — its range is no longer solicited), and
  cannot touch capability state; a late reply that instead lands inside
  the RETRY's window is clipped to it (§5.1: only the intersection with
  `solicitedPending` is solicited) and, if it EXTENDS beyond that
  window, changes no control state (§4.3's containment rule). One wasted
  fetch; safe.

## 5. Client changes

### 5.1 Store: from watermark doctrine to solicited-range doctrine

Today guard 2 rejects any line at or below `everEvictedThrough` ("evicted =
stale re-send forever") — pinned by the stale-re-send unit test in
`store.test.ts` (the property suite pins cap/window/idempotence/last-writer
invariants, not this guard; §7 adds a generated solicited/unsolicited flag
so the split is exercised, not asserted in prose). Paging is exactly a
legitimate re-fetch below that watermark, so the doctrine becomes:

- `solicitedPending` (exactly one slot, matching single-flight): the
  `[fromAbs, end)` window of the in-flight request, recorded before send.
  Lines inside it are applyable even below prior evictions. The
  solicited treatment applies to the INTERSECTION only: a correlated
  frame wider than the current slot (a late oversized reply racing a
  shrunken retry, §4.2) has its out-of-window lines routed through the
  ordinary per-line guards instead (typically dropped below
  `everEvictedThrough`) — the clipping is normative, not a test
  artifact. Cleared when the reply is applied, when the data timeout
  releases single-flight, on socket close, on epoch change, and on ED3.
  There is NO grace window beyond the timeout: a reply arriving later is
  dropped by guard 2 (wasteful, safe) — one slot, no set, nothing to
  expire.
- `browse`: the SET of absolute indices currently retained as browse
  cache — a subset of the store's own keys, holding exactly the rows
  received. This is the browse cache's MEMBERSHIP test, and it drives cap
  accounting, eviction (§5.3), and snapshot exclusion (§5.2), and nothing
  else. It is NOT a guard-2 bypass: re-delivery into a retained index
  passes only through `applyLine`'s idempotence (identical content) or
  arrives under a new `solicitedPending`.

  A set of KEYS, not of ranges, and with no companion counter. Ranges
  describe indices the store need not hold, which turned every question
  the cache is asked ("is the reader on cache?", "how many rows?", "which
  to evict?") into a walk over a numeric SPAN — and §5.2's replay-jump
  band spans every index a reconnecting client is missing, measured at
  1.2 s of main-thread work for a client 80M lines behind. Keys are
  bounded by residency, so every one of those questions is O(cache).
  `browse.size` is then the cache count by construction, which is what
  removes the counter and its arithmetic (§5.3).
- `everEvictedThrough` remains, for the UNSOLICITED case only (hostile or
  malformed frames below the floor), exactly today's semantics — with one
  interaction stated: it advances ONLY on tail trims (§5.3), and routine
  tail trimming pushes it ABOVE retained browse content; that is correct
  precisely because solicited applies bypass it and idempotent
  re-delivery tolerates it.

Naming: this is deliberately NOT called a "ledger" — that word is taken by
the reliable-INPUT ledger (`bytesReceived`/`ledgerLost`), which has nothing
to do with history validity. `ledgerLost` is NOT a paging trigger IN THE
ENGINE: absolute indices stay valid across a ledger loss and the store
keeps its history. One inherited consumer behavior is named beside that
rule so a test written from this section does not assert the opposite of
what the product does: the shipped ui's `onServerRestart` handler — which
the engine invokes on a ledger loss with unacked input
(`resetForgottenLedger`) — resets scrollback wholesale, dropping paging
state with it; the engine neither requires nor forbids that. The ENGINE's
paging-state drop triggers are boot-epoch change,
ED3/`scrollbackCleared`, and session switch; socket
close clears `solicitedPending` and capability state only (the cache's
indices are still valid).

Line identity makes the doctrine safe: within a boot epoch, an absolute
index is immutable server-side (ED3 and epoch changes clear, never
rewrite), so a paged-in line can never conflict with a retained one.

### 5.2 The retained-range model (why resume keeps working)

The store's held lines form an INTERVAL SET — as GEOMETRY, derived from the
retained key set, never as a stored structure: the live tail (window +
resident scrollback, normally one contiguous run ending at `highest`) plus
zero or more cached runs below it, with gaps between them. One
stated exception to the tail's contiguity, all of it pre-existing
behavior: under `unsupported` a stranded band stays tail-classified
(§4.5 step 4), so the tail can briefly hold TWO disjoint runs with a
hole inside it — today's shape, and `enforceCap` walks oldest-first and
reconverges to one block as live output arrives. Gaps are legal,
rendered as gap markers (§5.4 — the renderer today draws no interior
holes; that is new, scoped work), and healable (§5.4 fetches toward any
gap edge the viewport approaches, until `pagingFloor` says otherwise).

The resume contract is AMENDED in exactly one dimension: `haveThrough`
(the highest held index) is always in the live tail — window rows live in
the same map, `truncateBelowWindow` pins `highest` to the window bottom,
and the window is never evictable — and the replay now covers the NEWEST
`max(0, min(replayMax, committed − haveThrough − 1))` lines of
`[haveThrough + 1, committed)` (half-open; `committed` is one past the
newest, and `haveThrough` — an old window bottom — can legally sit AT or
ABOVE it when little or nothing scrolled while disconnected, hence the
outer `max(0, …)` — §4.5's bounded replay: a server that declares paging
clamps the START, never the end; an undeclared pairing replays in full).
It still never re-sends the gaps. A REPLAY JUMP — the replay landing
above the SENT `haveThrough + 1`, whether from the new clamp or from the
plain ring eviction every server produces today — is PREDICTED inside
the §4.5 ack transition, under `supported` ONLY (an `unsupported`
stranded band stays TAIL: today's behavior, today's bounds — see §4.5
step 4 for why the gate is load-bearing), and the prediction is computed
from the values the request CARRIED (`sentHaveThrough`,
`sentReplayMax`), never from the store's current `highest`: a pre-ack
live frame that slipped through before the drop rule existed — or any
future re-ordering — would move `highest` and mask the jump, leaving the
band tail-classified for `enforceCap` to eat; the sent values are what
the server's reply is actually a function of. The stranded band —
`[oldestHeld, sentHaveThrough]`, the OLD window rows included — moves
into `browse` (no line deleted, `everEvictedThrough` untouched,
the §5.6 TTL clock stamped), and the old window descriptor is RETIRED
(`win` empties; the batch's own window frame re-establishes it at the
new base — the jump band deliberately crosses the OLD `win.base` because
those rows stop being window rows the moment the new frame lands; the
cap flip's never-cross-`win.base` guard is about the LIVE window and is
not violated). Retirement carries three stated consequences, all bounded
by the batch: the alt gate's frozen-base comparison is VACUOUS while the
descriptor is empty (safe — on the alt-first batch order every replay
line sits strictly below the incoming base, the accept condition §10 #8c
expresses, and `writeMu` keeps foreign writes out of the batch); no
window-derived bound may be EVALUATED while retired
(`truncateBelowWindow`'s bottom would read −1 and delete the store — its
only call site runs after `updateWindow` re-establishes the descriptor,
and that pairing is now a stated requirement, not an accident); and one
renderer flush CAN land between the ack task and the window-frame task
(the platform queues one task per message), drawing the cursor overlay
from the retired descriptor for a single frame — a visual artifact, not
a correctness one, named so "no DOM touched" is not read as absolute.
Ordering is the point of the whole arrangement: the batch's window frame
lands FIRST on the main-screen path, and each of its `applyLine` calls
runs `enforceCap` — against a still-tail-classified band under a freshly
flipped cap, that pass would delete band lines and advance
`everEvictedThrough`, exactly what this paragraph promises cannot
happen. After the jump step the live tail is again ONE contiguous run
ending at the new `highest`, the stranded content stays readable as
ordinary browse cache under the §4.5 budget pass, and the gap between
renders markers and heals on approach like any other. (A pre-existing
nuance, unchanged by this design: `haveThrough` is the old window
BOTTOM, so a row that changed while disconnected and then scrolled into
history is not re-sent by replay. Paging neither creates nor fixes that;
noted so nobody attributes it here.)

Persistence: the browse cache is EXCLUDED from `snapshot()` BY
CLASSIFICATION, not by contiguity — serialization walks down from
`highest` and stops at the first index that is absent OR a member of
`browse` (a fetched page can sit flush against the tail with no
numeric gap; contiguity alone would serialize it). A hydrated store is
therefore always one contiguous tail and its `everEvictedThrough =
oldest − 1` remains accurate. (Persisting the cache would spend IndexedDB
writes on data the design defines as disposable, and would hydrate
interior holes.)

### 5.3 Residency model and eviction

The store keeps ONE map of retained lines plus the `browse` key set as the
classification — no per-line flags, and no parallel range structure.
`oldestHeld` is DEFINED as the global minimum retained key
(`oldestIndex()`, browse included): the frontier reads it; the tail budget
reads `tailCount`.

NEITHER count is maintained. `browse.size` is the cache count and
`lines.size − browse.size` is `tailCount`, both read straight off the two
containers, so no mutation path owes either of them an update and no
overlap can double-count. The design tried the alternative first: a
`browseCount` maintained by arithmetic at every mutation, which was wrong
at four of them — it reached −96 on a third successive jump ack, and the
under-report then had the tail budget evicting live rows to pay for cache
the store did not believe it held.

That leaves exactly ONE invariant for the mutation paths to keep,
`browse ⊆ lines.keys()`, and one place that keeps it: `forget(abs)`, the
sole owner of deletion in the store, which removes the line and its
classification together. Every path that drops rows goes through it —
browse eviction, `dropBrowseCache`, tail eviction, ED3's
`applyScrollbackCleared`, and `truncateBelowWindow` (which fires on every
soft-keyboard resize, and was one of the two paths the old parallel
structure did not maintain). `fromSnapshot` hydrates with an empty cache
by §5.2's exclusion, which is what keeps its `everEvictedThrough =
oldest − 1` derivation accurate. The numeric invariants, each enforced
independently:

```text
tailCount   <= max(effectiveTailCap, windowHeight)   // today's tail rule,
                                               // on the effective cap
browse.size <= browseCacheCap
total       <= max(effectiveTailCap, windowHeight) + browseCacheCap
```

The invariants are enforced at OPERATION boundaries: a full-page apply
raises `browse.size` by up to `pageSize` before its own budget pass runs,
so the store-line peak inside that window is `steady + pageSize` (5000 at
the defaults — equal to today's cap, held for the microseconds between
apply and pass, never sustained; §3's 4000 is the steady bound). An
implementer must not assert the browse budget INSIDE a bulk apply.

- The RESIDENT TAIL cap is `effectiveTailCap`, a MUTABLE store field —
  the one deliberate exception to the constructor-fixed shape, because
  §4.5's capability outcome must change it at runtime. Constructor-knob
  (`scrollbackLines`) semantics, defined fresh here (the knob is
  unreleased): OMITTED → `compatibilityCap = 5000`,
  `supportedTarget = 1500`; SUPPLIED N → both are N — an explicit value
  is a memory decision and holds in EVERY capability state (the flip is
  then a no-op). `effectiveTailCap` starts at `compatibilityCap` and
  flips ONE-WAY to `supportedTarget` via the ack transition's cap-flip
  phase (§4.5) when the
  resumeAck declares paging (§4.5). Socket replacement does NOT revert it
  (each socket's ack re-declares capability, and a session reconnects to
  the same server process); only a boot-epoch reset — a genuinely
  different server instance — returns the store, cap included, to the
  compatibility state. EVERY residency reader uses the effective value: the tail-budget
  gate, `evictionBatch(effectiveTailCap)` (256 at 5000, 93 at 1500), and
  `snapshot()`'s default bound; `fromSnapshot` already trims to the
  newest cap, so a snapshot taken under the compatibility cap hydrates
  correctly into a fresh store (which itself re-starts at the
  compatibility cap — capability is per-socket). `browseCacheCap` is an
  ENGINE-INTERNAL constant (2500) — not a consumer knob, not a
  constructor argument; consumers keep the single-argument constructor
  and factories they have today.
- The CAP FLIP — `confirmPaging`, now a PHASE of the §4.5
  `applyResumeAck` transition rather than a separately callable method —
  is two steps, bookkeeping only: (1)
  set `effectiveTailCap = supportedTarget`; (2) RECLASSIFY the excess
  tail — keep the newest `max(supportedTarget, windowHeight)` tail lines
  and NEVER cross `win.base` (a LIVE window row is never reclassified;
  §5.2's replay-jump band deliberately includes the OLD window rows
  because it retires the descriptor first) —
  moving older tail lines into `browse` (no
  line deleted, no DOM touched, `everEvictedThrough` untouched, and the
  §5.6 TTL clock STAMPED so the band's countdown starts at the flip).
  The flip runs NO budget pass of its own: draining belongs to the
  transition's final step — ONE viewport-aware pass over the WIDER of
  the bands the flip and the jump reclassified. They are NESTED by
  construction (both begin at `oldestHeld`; the jump band's top edge,
  `sentHaveThrough`, is at or above the flip band's), so "wider" is
  exact and no interval-union machinery is implied. That ownership is
  load-bearing, not stylistic: the two readings diverge (on a jump whose
  band contains the viewport, a flip-local pass would see the viewport
  OUTSIDE its smaller flip band and delete 3000 lines at the 500 target
  before the jump step had told the store what the band actually is;
  the wider-band pass sees the viewport inside and keeps 2500). The
  pass's
  TARGET: `prefetchThreshold` (500) when the band does not contain
  `viewportAbs` — the band is disposable, and only a scroll-up buffer
  needs to stay hot — or the full `browseCacheCap` when it does (protect
  the deep reader; the TTL cleans up later). CONTAINMENT is defined
  precisely, because a jump band spans the whole store and can span
  numeric holes: a FOLLOWING viewport is OUTSIDE every reclassified band
  by definition (it is looking at the live tail, not at cache — the
  common case, and the one §3's memory picture is keyed on), and any
  other viewport is a reader in HISTORY, whose depth the pass protects.
  **FOLLOWING IS AN INPUT, not something the store derives**, and that
  is a correctness requirement rather than a stylistic one — the store
  cannot answer it, and each of the two ways it tried was wrong in a
  different direction:
  - From `viewportAbs >= win.base` it read a descriptor step 4
    deliberately RETIRES. The jump band is
    `[oldest, sentHaveThrough + 1)`, the client sends its HIGHEST
    retained index as `haveThrough`, and that index is the window's
    bottom row — so the band spans the old window rows and a following
    reader sits inside it. With the descriptor gone the derivation
    answered "not following" for every predicted jump and the pass kept
    `browseCacheCap` on the exact path this section names as the primary
    population: about 2000 lines over budget. (A test written with a
    `sentHaveThrough` BELOW the window base cannot see this — it excludes
    the reader from the band and passes against the bug.) Sampling the
    value before step 4 patches the instance and leaves the hazard.
  - Narrowing it with "and `viewportAbs` sits on a row the pass just
    reclassified" then broke the opposite direction. Membership is a
    proxy for "reading history" and it fails wherever the reader's index
    is not currently held — a hole inside the cache, or an ARMED RESTORE
    anchor whose row has since been dropped, which is precisely the
    anchor the renderer prefers over a mid-rebuild measurement
    (docs/scroll-position-fidelity.md §7.2). Both are ordinary, and both
    drained a deep reader's depth to `prefetchThreshold`.

  The renderer has one unambiguous source — its own scrolled-up state,
  overridden by an armed restore, which means "in history at the anchor"
  — so it PASSES the answer and the store stops guessing. Retiring the
  window descriptor mid-transition then cannot corrupt anything, which
  is the difference between removing this defect class and patching its
  latest instance.
  Eviction at the
  ack
  boundary therefore
  happens only through the far-edge, viewport-exempt browse rule, in
  `pageSize`-sized far-edge removals: a full 5000-line legacy tail
  reclassifies 3500 lines and, with a following viewport, drains 3000 in
  three removals — synchronous STORE removals; the renderer drains the
  evicted set on its next flush, far from the viewport (pre-existing
  behavior class: a large resume replay already
  evicts unchunked through the same drain loop) — keeping approximately
  the newest 500 flush against the tail as the scroll-up buffer.
  APPROXIMATELY, because the drain stops at the viewport exemption, which
  spares `prefetchThreshold` on EITHER side of the reader: the honest
  ceiling is the `2 * prefetchThreshold + 1` invariant this section
  already states (~504 lines observed), not the target itself. The rows under a
  deep-scrolled reader sit inside the viewport exemption and SURVIVE the
  flip. WHO RUNS IT: `connection` is store-blind and viewport-blind, so
  the port's `applyResumeAck` member is a single closure BUILT BY THE
  CONSUMER that supplies both viewport facts — the reader's index (the
  same source `applyHistoryScroll` uses, or an armed restore's anchor)
  and whether that reader is FOLLOWING — and forwards the
  connection-captured ack fields and sent values (§4.5) into the store's
  one transition — the
  five steps cannot be split across layers because only one call
  crosses them. WHEN the flip band is non-empty in
  practice: almost never — capability is declared at every attach's ack
  (§4.5), so on a supported server the tail cannot outgrow the target
  between attaches; the real population is the FIRST attach over a
  legacy 5000-line snapshot hydrated at `compatibilityCap`, plus defence
  in depth. A reclassified band is thenceforth ordinary browse cache:
  snapshot-excluded (§5.2 — a snapshot right after the flip persists only
  the tail; the band is re-fetchable, which is the design), TTL-subject,
  and evictable. §7 pins the transition order, the window floor, both
  drain targets, the nested-band rule, the following-viewport
  containment rule, and the reading-position assertion
  (viewport parked
  mid-band at the flip — the anchored index is still on screen, its rows
  reclassified, not evicted).
- Enforcement is TWO separate mechanisms, never one pooled walk:
  - Tail budget: today's `enforceCap` walk with THREE amendments — (1)
    its gate and victim count use `tailCount`, not `lines.size`; (2) the
    cursor starts at `this.oldest` (the O(1) global minimum, exactly
    today) and the HOP predicate extends to browse intervals — the walk
    hops over them the way it hops the window, which keeps today's
    `oldest`-maintenance line correct (`this.oldest` stays the global
    minimum; hops trigger the existing bounds recompute); (3) only tail
    victims advance `everEvictedThrough`. Live output while browsing
    therefore CANNOT touch the browse cache, and browse pressure CANNOT
    touch the tail.
  - Browse budget: enforced at page-apply time only. The apply path is a
    new bulk entry point `applyHistoryScroll(msg, viewportAbs)` — the
    ALT GATE first (identical to `applyScroll`'s prologue; §5.5 banks on
    it), then guards 1–5 per line with guard 10 (per-line `enforceCap`)
    SUPPRESSED, then exactly the keys it stored join `browse`, then ONE
    browse-budget pass:

    ```text
    need = browse.size − target                // target = browseCacheCap
                                               // here; a reclassify pass
                                               // may pass 500 (§4.5/§5.3)
    end   = whichever END of `browse` is farther from viewportAbs
    remove = min(need, pageSize, nonExemptRunAtThatEnd)
    ```

    removing exactly the overflow (a one-line overflow removes one line,
    not a page — anything larger thrashes fetch/evict cycles at the cap)
    and EXEMPTING all lines within `prefetchThreshold` of `viewportAbs`
    (an eviction must never create a hole the trigger immediately
    re-fetches, nor blank the rows under the reader). If that end yields
    nothing the pass STOPS and accepts a bounded overshoot rather than
    spin — the stop rule is TARGET-AGNOSTIC.

    Direction is computed from the cache's own extremes, never from a
    range's edges. Asking whether a range BEGINS at or above the viewport
    to find its far side is false whenever the reader is INSIDE the cache,
    which is the normal deep-scroll shape, and the answer inverts: victims
    come from the low end, the direction an up-scrolling reader is
    heading, while the pages behind them are never freed.

    Termination is structural: every victim is a held row and every removal
    shrinks the set the loop tests, so the pass cannot fail to make progress
    the way a range-edge walk could (removing a HOLE at a range's edge
    changed nothing, and the walk re-picked it forever — an infinite loop on
    the main thread, reachable from an ordinary reconnect).

    The overshoot IS reachable, at the small reclassify target only: the
    exempt band is `2·prefetchThreshold + 1` wide, so a cache SPANNING the
    reader but narrower than the band is entirely exempt while still over a
    500 target (two 400-row runs either side of the reader do it). Nothing
    is evictable, the pass stops, and the cache stays — the trade the
    exemption exists to make. Under `browseCacheCap` the same arithmetic
    never bites, since `1001 < 2500`. Browse eviction removes membership
    and lines together and NEVER advances `everEvictedThrough` (a cache
    drop is not a permanent trim).
- `viewportAbs` is supplied by the renderer (the only layer that knows
  it) on every `applyHistoryScroll` and `dropBrowseCache` call; the store
  never guesses viewport state.

New store API (all consumer-visible pieces listed in §8's rollout):
`applyHistoryScroll(msg, viewportAbs)`, `noteSolicited(fromAbs, end)` /
`clearSolicited()`, `applyResumeAck({ epochChanged, committed,
serverOldest, paging, sentHaveThrough, sentReplayMax, viewportAbs })`
(the §4.5 transition — ONE store entry point for the five ack steps;
`confirmPaging` is its cap-flip phase, not separately callable; the §5.4
port wraps it in a consumer-built closure because the ack is decoded in
`connection`), `intervals()`
(or `absentEdgesNear(abs, threshold)`) for the trigger,
`serverOldestIndex()` (today a private field only `hasTrimmedHistory()`
reads), `pagingFloor()` accessors (owned here, §4.5),
`dropBrowseCache(viewportAbs, pageVisible)` (clears browse intervals and
their lines — but SKIPS and re-arms when the page is VISIBLE and
`viewportAbs` currently sits on browse rows: the §5.6 TTL is an
inactivity signal, and a visible reader parked on a long stack trace is
inactive while looking straight at cache content; wiping the rows under
the viewport and re-fetching them one RTT later serves nobody. A HIDDEN
page has no reader, so it drops UNCONDITIONALLY — without the visibility
condition the skip would retain exactly the deep-scrolled cache the
hidden-page TTL exists to free, since a deep-scrolled viewport sits on
browse rows by definition; visibility comes from the consumer with the
call, the store never reads the DOM), `lastBrowseActivityMs()`.

### 5.4 Fetch controller, triggers, and markers

The controller lives in `render.ts` — the one module that already maps
viewport position to absolute indices (the read-anchor machinery reads
`data-abs` off DOM rows). It is invoked from (a) the post-flush anchor
path and (b) a NEW per-scroll-event seam on `scroll.ts`:
`onScrollPosition?(): void`, fired from the existing scroll handler AFTER
its `preserveFollowOnce` early-return (that arm absorbs the library's own
programmatic scroll writes — a paged-in prepend goes through
`adjustForContentShift`, and firing the seam before the early-return
would turn every prepend into a fresh trigger). Today's only callback
fires solely on follow/hold TOGGLE, so without this seam paging would
never trigger on an idle session — the primary use case. `scroll.ts`
stays transport-free and index-free; the seam is a bare notification.

Transport is reached the way the view already reaches it — consumer-wired
callbacks, not a render→connection import: `connection` exposes
`requestHistory(fromAbs, maxLines)` (owning single-flight, pacing + the
pending-demand timer, the adaptive budget, and close-time cleanup) and
`historyBudget()` (the current `effMax`, read by the §5.4 trigger at fire
time — the controller lives in `render.ts` and this accessor is its only
channel to connection-owned state; without it the anchor arithmetic
cannot use the shrunken value and a 125-line retry would land 875 lines
from the reader),
and the consumer passes both into `render.init` options exactly like
`getHaveThrough` flows the other way. Because `connection` is store-blind
by design, `requestHistory` is handed a small PORT at wiring time —
`{ noteSolicited, clearSolicited, applyResumeAck }` — so the events that
originate in `connection` and must reach the store can: data timeout,
socket close, and capability reset clear `solicitedPending`; the resume
ack routes through `applyResumeAck` (§4.5's one transition — a closure
the CONSUMER builds to capture the renderer's viewport getter and
forward the connection-captured ack fields and sent values, §5.3, so
`connection` knows neither the store nor the viewport).
Correlated replies route to the consumer through a new
`onHistoryReply(msg)` callback, which
the consumer's message path feeds into `render`/`applyHistoryScroll` (the
ui minor wires callback, port, and seam; §8).

Gap geometry is half-open and normative: for retained intervals
`A = [aLo, aHi)` and `B = [bLo, bHi)` with `aHi < bLo`, the gap is
`[aHi, bLo)`. The bottom frontier is the pseudo-gap
`[pagingFloor, oldestHeld)` (`oldestHeld` = the global minimum retained
key, §5.3). The pseudo-gap can be EMPTY or INVERTED
(`pagingFloor >= oldestHeld`, e.g. after an empty frontier reply); the
trigger's `gapHigh > pagingFloor` guard makes it unreachable, and the
request arithmetic below is evaluated only AFTER the guard set passes
(the `gHi − fromAbs` subtraction would otherwise underflow). The store
is the authority for absent edges, and the SOURCE is the retained KEY
SET, not `browse` (the two differ in exactly one shape — the
`unsupported` two-run tail of §5.2, where a hole sits inside the tail
rather than below it; reading cache membership would draw adjacent rows
across that hole with no marker, silently splicing unrelated regions,
which is the misrepresentation gap markers exist to prevent). Because the
trigger asks on every scroll event and the gap markers ask on every
flush, while the key set changes far less often, the coalesced ranges are
MEMOIZED and invalidated by the two owners of key-set mutation — measured
95 us per uncached call at 4500 rows, so 5.7 ms per 60-event scroll
frame. The DOM lags a
draining page by a few frames; the controller re-fires harmlessly under
pacing.

Trigger rule:

```text
fire iff  alt is NOT active               // §5.5; load-bearing for the
                                          // §4.4 timer, which re-runs this
                                          // guard from a clock — a vim
                                          // session must not fetch
      and capability is `supported` (§4.5; pre-ack nothing is legal)
      and client pacing bucket has a token (else arm the §4.4 timer)
      and no request in flight
      and the viewport is within prefetchThreshold lines of an absent
          range's edge
      and gapHigh > pagingFloor            // NOT gapLow: the frontier's low
                                           // edge EQUALS the floor in the
                                           // steady state after any ring
                                           // exhaustion + later tail trim,
                                           // and that reopened frontier must
                                           // stay fetchable
      and for the bottom frontier additionally: oldestHeld > 0 and
          (serverOldest unknown or oldestHeld > serverOldest)
          // serverOldest is always known by the time a request is legal
          // (resumeAck precedes it, §4.1); the unknown arm is defence in
          // depth, not a live path. It is a stale-LOW bound refreshed only
          // at resume; the authoritative stop is the reply clamp/empty.

request (approach-anchored — always fetch the END NEAREST the reader;
`effMax` is the §4.2 adaptive budget, `pageSize` when the link is
healthy):
  gap [gLo, gHi), viewport approaching from ABOVE (scrolling up):
      fromAbs = max(gLo, gHi - effMax, pagingFloor)
  viewport approaching from BELOW (scrolling down):
      fromAbs = max(gLo, pagingFloor)
  maxLines  = min(effMax, gHi - fromAbs)
```

(The approach-anchored rule is what makes a wide gap heal from the
reader's side; fetching a fixed end would land pages up to
`gapWidth − pageSize` lines away from the viewport. The `pagingFloor`
clamp keeps a floor-straddling gap from re-requesting known-gone lines.
One visible consequence of §4.2's prefix-shrink: a byte-short reply at
the frontier lands its lines at the WINDOW'S OLD END and opens a fresh
sub-gap directly below the reader until the next paced continuation
fills it — a two-step the marker shows honestly as a brief loading gap.)

Markers: per-gap marker ELEMENTS (not the singleton top trim marker),
each carrying its gap's LOW index as `data-abs` so `insertRowInOrder`
and the read-anchor binary searches keep their monotonic-`data-abs`
invariant (this ends the "non-row children read −1 and sort first"
property those searches document — update that comment in the same
change). A marker is a PROJECTION of the interval set: re-derived (value
and position) whenever either edge of its gap changes — a gap healing
from its LOW edge moves the marker's `data-abs` up with it — and removed
when the gap closes. Gap markers exist WHEREVER GAPS EXIST, independent
of capability: a reconnect that kept a gapped cache under
`capability == unknown` still renders its gaps (rows at 4000 abutting
rows at 18000 with nothing between would silently splice two unrelated
regions — the exact misrepresentation this design exists to prevent).
Capability gates only the marker STATES, and the state set is THREE, the
idle one named because it is the DEFAULT: (1) "earlier output not
loaded" — asserts a hole while claiming no activity; the state of EVERY
gap in EVERY capability state unless overridden, including the pre-ack
instant and the supported-but-nothing-in-flight common
case (a gap must never read as contiguous, and the top marker's neutral
— "assert nothing" — is not available to interior gaps); (2) "loading
earlier output…" while a correlated request for THAT gap is in flight;
(3) permanent "earlier output trimmed" when
`pagingFloor` covers the gap, or for any gap while `unsupported`
(unfillable is unfillable). A PARTIALLY condemned interior gap
(`pagingFloor` strictly inside it, §4.3) shows state (1) for its
fetchable remainder; the condemned sub-range's permanence surfaces once
the remainder heals and the floor covers what is left — accepted
cosmetic staging, stated so the marker's "gone vs not yet" channel reads
correctly. No reserved
height (a marker is one line tall; reserving the gap's height would make
the scrollbar lie). The top-of-store trim marker remains the special
case at the absolute top, with an EXPLICIT predicate under `supported`:
permanent "earlier output trimmed" when `pagingFloor >= oldestHeld` (the
frontier is exhausted or condemned — §4.3's empty-frontier raise lands
exactly here) OR `serverOldest >= oldestHeld` (the trigger's own
frontier stop, mirrored — reachable in ordinary steady state when a
supplied `scrollbackLines` exceeds the ring depth, since the client then
accumulates more than the server retains; without the disjunct the
marker would claim "not loaded" forever about content that can never
load), state (1) otherwise. The frontier pseudo-gap is EXEMPT
from "removed when the gap closes": an empty or inverted frontier gap
still renders the top marker under this predicate, so reaching the true
top of history shows a permanent trim marker, not a vanishing one. Under
`unsupported` — and in the pre-ack instant, which wants the same answer — the
marker falls back to today's `hasTrimmedHistory()` predicate. "Neutral"
pre-ack is wrong and was implemented as the fallback instead: a client that
evicted its own oldest rows at the cap has trimmed history whatever the
server can serve, so suppressing the marker until an ack lands would hide a
fact the client already knows. That collapse is also why the store exposes
`pagingDeclared()` as a BOOLEAN rather than a tri-state — the third state
had no distinct behavior to justify it.

Top insertion: paged-in rows prepend above the current content. The
read-anchor machinery corrects by measured on-screen drift, not content
delta, so insertion is the same arithmetic as trim; extend its tests to a
1000-row page draining across multiple render frames (the render queue
processes ~300 rows/frame), not one synchronous prepend.

### 5.5 Interactions

- Alt screen: no paging while `alt` is active. The fetch controller's
  event paths are scrollback-UI-only and unreachable in alt, and the
  CLOCK path is closed explicitly: alt activation cancels the
  pending-demand timer and the trigger guard refuses to fire in alt
  (§4.4, §5.4 — a timer surviving into a vim session would otherwise
  fetch pages nobody can see, and each denial would re-arm it for the
  session's whole duration). On alt exit the store rebuild re-derives
  gaps and the ordinary event-driven trigger takes over. A page reply
  that lands during an alt flip applies under the shipped alt gate
  (history below the frozen main `win.base` stores; at/above drops —
  §10 #8c).
- ED3 / `scrollbackCleared`: drop the history below the cleared bound,
  cancel `solicitedPending`, snap `pagingFloor` to that bound (§4.5), and
  record the highest index actually dropped as the ERASED watermark. The
  cancel and the snap run even when this client holds NOTHING below the
  bound — an in-flight request and a stale floor are not conditional on
  local residency, and the early return covers the line drop only.
  - Handled BEFORE the screen-mode dispatch, because the signal is a
    property of the SESSION and not of the buffer the carrying frame
    describes. The server raises a pending flag in the PTY path and attaches
    it to whatever frame it builds next with no alt gate, so handling it on
    the main-screen path alone made every alt-active ED3 a no-op — reachable
    from `clear; vim foo` typed as one line. The rows-less signal check sits
    with it, so a zero-row frame cannot collapse the alt grid either.
  - The ERASED watermark is what refuses a re-delivery, and it is separate
    from `everEvictedThrough` because only that one means "trimmed for
    memory" and drives the marker; ED3 must not raise a marker on an app
    that clears every redraw. It tracks what was DROPPED rather than the
    cleared bound, because the gap between them is exactly where the app's
    own reprint lands (new commits below the new window base, which must
    apply). Refusing an erased index is safe for the epoch's life: the
    server's ring `Clear()` preserves `committed`, so an erased absolute
    index is never reused.
- Boot epoch change: full reset already drops everything; the browse set,
  `solicitedPending`, `pagingFloor`, and capability state join it.
- Socket close/replace: clear `solicitedPending`, single-flight,
  capability (back to pre-ack), and the adaptive budget (`effMax` to
  `pageSize`, the remembered timeout size with it — §4.4's atomic list);
  KEEP the browse cache — its indices are
  valid within the epoch. Stale-reply safety needs no per-frame epoch
  check (scroll frames carry none): the old socket closes before the new
  one resumes, frames are FIFO per socket, and a post-reconnect stray
  correlates against nothing. What refuses a stray that DOES correlate is
  the page-apply path itself: with no in-flight window it drops the frame
  whole. Guard 2 cannot be what refuses it — the watermark deliberately
  does not advance on ED3, and at the store level a stale page is
  indistinguishable from the legitimate content an app reprints below its
  new window base after clearing (which must apply). Only the caller knows
  the frame was correlated against a window that no longer exists.
- Resume racing a page reply: both are absolute-indexed and idempotent;
  `writeMu` serializes the write sequences.

### 5.6 Browse-cache lifetime (owner decision 2026-08)

The browse cache is disposable by construction (recovery = one page
fetch). It exists when the user deep-scrolls, and — bounded and stamped —
when a RECLASSIFY creates it (§5.3's flip band, §5.2's replay jump; both
stamp the TTL clock at creation, so the band's countdown starts then).
It is evicted by
a uniform 5-minute inactivity TTL — never eagerly — so rapid switching in
any direction stays instant:

- ONE periodic sweep covers EVERY store the consumer owns, not just the
  one currently bound to the renderer. The renderer only ever sees the
  bound store, so a sweep written against it reaches the visible tab and
  nothing else, and every background tab's cache lives as long as the page
  — up to `browseCacheCap` rows each. The consumer's store factory is the
  one place every store passes through, so that is where they become
  reachable (weakly, so a closed tab's store stays collectable).
- The BOUND store's drop is conditional and goes through the renderer, the
  only layer that knows where the reader is: it skips and re-arms while the
  viewport sits on browse rows (inactivity is not off-screen-ness, and a
  reader parked on a stack trace must not have the rows under them wiped).
  Every OTHER store has no reader, so there is no position to protect and
  its cache drops unconditionally once idle.
- Page hidden (app switch, other Safari tab): nothing special. A hidden
  page's timers are throttled and a frozen page runs no code at all, so the
  sweep simply resumes on return and drops within its next tick.
  Enforcing the hidden period's debt ON the return transition was tried and
  REMOVED: it necessarily ran with hidden-page semantics
  (unconditional), which deleted the rows the returning reader was parked
  on in the one moment they are certain to look at them — and it bought at
  most one sweep interval over doing nothing.

The FROZEN page — the one state the sweep cannot reach, since a frozen page
runs no code at all — is covered by a last-chance hook that drops every
cache UNCONDITIONALLY, TTL ignored: `freeze` (Chrome's Page Lifecycle
signal) and `pagehide` with `persisted` (entry into the back/forward cache,
the Safari path on the platform this is for). Either can fire without the
other, so both are wired.

Unconditional is right there and wrong on the return transition, and the
difference is which way the reader is walking. `visibilitychange` to
visible fires as a reader ARRIVES, so a drop there deletes the rows they
are about to look at — that drop was implemented, measured against this
distinction, and REMOVED; it bought at most one sweep interval over the
periodic pass. `freeze` fires as the page stops being a running program,
with no reader and none imminent, and holding 10–20 MB (estimate) for an
unbounded freeze that may end in a discard serves nobody. The cache is
disposable by construction, so the cost of being wrong is one fetch on a
return that may never come.

Accepted residual: a page killed without any lifecycle event (an OS-level
kill, a crash) frees its memory with the process, so nothing is left to
reclaim.

Ownership: the engine provides the mechanism (`dropBrowseCache()`,
`lastBrowseActivityMs()` on the store, surfaced through the renderer
binding); the CONSUMER owns the timers and visibility hooks (the engine
has no notion of tabs or visibility). All kiro-internal tabs share ONE
page and one JS context, so the foreground code manages every internal
tab's cache — the TTL is fully reliable there. Separate SAFARI tabs are
isolated processes: a frozen one runs no code and cannot be reached from
the foreground tab (messages queue until it thaws), so cross-Safari-tab
cache management is impossible by platform design. Multiple sessions
belong in kiro's internal tabs, not in multiple Safari tabs.

The resident tail (1500) is exempt from the TTL: it is the reconnect/live
contract, not a cache.

## 6. What this deletes

- The phone-vs-desktop cap decision: 1500-resident everywhere; depth comes
  from the server ring, not client residency.
- The "raise the cap" pressure on `maxLines`: depth is now a server-side
  `WithScrollbackCapacity` number (kiro ships 20000, §3) at zero client
  memory cost; any future "deeper please" is a one-line server change.
- The `scrollbackLines` knob as a user decision (it survives, compatible,
  as the resident-tail override; the effective default flips 5000 → 1500
  as the POST-ACK steady state, one RTT after attach on the resumeAck's
  `historyPaging` bit — §5.3's compatibility cap keeps the legacy 5000
  whenever paging is not declared, so an old-server pairing never
  regresses reachable history).

## 7. Testing

- Property tests: extend the store generators with a per-batch
  solicited/unsolicited flag so the guard-2 split is GENERATED (today's
  suite pins cap/window/idempotence/last-writer invariants; the stale
  re-send rejection lives in a unit test). New invariants generated
  together: tail contiguity (scoped to `supported` — an `unsupported`
  eviction-gap resume legally holds two disjoint tail runs, today's
  shipped shape, bounded by the compatibility cap; §4.5 step 4's gate),
  tail budget, browse budget, total bound,
  window protection, and `oldestIndex()` EQUALS the minimum retained key
  hold independently under interleaved pages/frames/evictions (the last
  is the invariant a lying `this.oldest` slips past — r3's cursor defect
  was invisible to every other listed property); `haveThrough` stays in
  the live tail while browse intervals exist.
- Server unit: `LinesRange` (clamp, bound, empty); the serving arithmetic
  guards (`fromAbs > committed`, request entirely below ring oldest →
  empty reply with `firstIndex = fromAbs`, NEVER lines outside the
  request window; the §4.1 subtraction-form guard incl. the wrapping case
  `fromAbs` near int64 max, and the boundary
  `fromAbs = 2^53 − 1 − maxLines`); the per-row URI-strip ceiling (an
  adversarial `maxResizeCols`-cell alternating-OSC-8 row — URIs at the
  `maxOSCLen` cap, so each URI is `maxOSCLen − 3` bytes — the `8;;`
  introducer shares the buffer — and the pre-strip size is pinned to the
  EXPRESSION `19 + 2 + maxResizeCols × (18 + 1 + (maxOSCLen − 3))`
  (= 4,112,021 today), not the literal — is stripped and fits; a
  stripped row
  preserves text + styling, empties `U`, AND clears attr bit 1024;
  byte-identity of the same committed row across page, live, replay, and
  screen paths; the header-inclusive boundary pair 262,125/262,126;
  `runCount ≤ cols` asserted before relying on the 22 KB proof; the
  arithmetic pinned to `maxResizeCols`, not the literal 1000; one
  `encodedRowSize` helper drives strip, packing, and these tests);
  byte-budget shrink keeps a PREFIX (mutant:
  shrink from the low end → the reply reads as a clamp and a client test
  must fail on the false `pagingFloor` raise); request validation
  (negative/fractional/over-2^53/overflow inputs dropped); the history
  token bucket incl. server slack under compressed arrival spacing;
  pre-resume requests ignored; the 10 s write context; the resumeAck
  tail bit (set only when the control is served AND
  `scrollbackCapacity ≥ paginationMinRing` — the boundary pair against the
  CONSTANT, plus the INVARIANT that couples it to `maxReplayLines`: every
  capacity the replay can truncate declares paging, and no capacity at or
  below the bound does (the independent pair stranded 2001..4999);
  length-gate shapes: short tail, bit0-only flags); the
  bounded replay (`replayMax` sent already CLAMPED to `maxReplayLines`
  (2000) so the sent value equals the honored value — the jump
  prediction depends on that identity; the server clamps too, as
  defence against other clients; decoded ADVISORY-safe — absent → full
  replay, today's behavior for every old client; malformed
  (fractional/string/overflow) reads as absent and the resume still
  SERVES, never a dropped control; present `< 1` treated as absent;
  honored ONLY when
  bit1 is declared, so an unsupported pairing replays in full;
  `replayFrom = max(haveThrough + 1, committed − replayMax)` incl. the
  `committed < replayMax` underflow guard and `haveThrough = -1`; the
  half-open count — `committed = 100`, `haveThrough = 99` → zero lines,
  and `haveThrough ≥ committed` → zero, never negative;
  the jump shape — a client 20000 indices stale receives exactly
  `replayMax` lines; the supplied-cap shape that broke the unclamped
  version — `scrollbackLines: 10000`, client 3000 lines behind: sent and
  honored are both 2000, the prediction and the real `replayFrom` agree,
  and the jump is detected).
- Ordering: extend `resume_ordering_test.go` — a page reply must not
  interleave into a resume batch (regression guard; the goroutine model
  already serializes them).
- Client unit: correlation edges (a frame at `fromAbs + maxLines` must
  not correlate; a clamped-but-in-window reply must; live-shaped frames
  with `firstIndex` at the PREVIOUS window base — strictly below the
  current one, the shape the server actually emits — never release
  single-flight); clamp/empty replies raise `pagingFloor` and the trigger
  respects `gapHigh > pagingFloor` INCLUDING the reopened-frontier
  equality case (empty-reply raise, then a tail trim, then a successful
  re-fetch of `[floor, oldestHeld)`); the floor's full lifecycle (ED3
  snap; epoch reset BEFORE same-ack bounds application, tested through a
  restart with a stale high floor; resumeAck lowering); a byte-short
  reply is NOT a terminator; trigger guards (`fromAbs` never negative or
  below the floor, `oldestHeld == 0` no-fetch, gap WIDER than `pageSize`
  approached from each side — the shape where approach-anchoring and
  fixed-end arithmetic actually diverge — plus narrower-than-page gaps);
  capability from the ack tail (bit1 set → `supported` and the
  consumer-built `applyResumeAck` closure runs the §4.5 transition; bit1
  unset or tail
  absent/short → `unsupported`, and NOTHING is ever sent; a
  never-scrolled store still flips to 1500 — no scroll, no request;
  reconnect re-reads the bit and re-arms pacing + single-flight +
  `effMax` together; the transition is ONE store call with NULLABLE
  bounds — epoch reset
  reverts the cap, then
  bounds (keeping `committed`), then the flip, then the JUMP prediction
  from the SENT values, then one WIDER-BAND budget pass, ALL BEFORE any
  batch
  frame applies AND before
  the ledger-loss/forgotten-session early returns (the ledger-lost
  attach is the long-absence one that carries a jump — capability and
  the jump must not be skipped on it); the 9-, 17-, 33-, and 35-byte ack
  shapes (a bounds-less ack still resets on an epoch change and still
  reads `unsupported`, and SKIPS the bounds and jump steps — never
  substituting zero bounds); the transition SUBSUMES the shipped
  `onResumeBounds` callback (not wired alongside it) and runs AFTER the
  consumer's epoch `onServerRestart` reset (a reset landing after would
  revert the cap under a `supported` connection);
  PRE-ACK frames are FIELD-AWARE
  (row and window mutation suppressed — no `highest` movement, no
  `enforceCap` under the stale cap; `scrollbackCleared` still ROUTED to
  the store clear — the resume window frame hard-codes it false, so
  dropping it would leave the client showing discarded history; the
  piggybacked `inputAck` still applied; `bell` dropped by design;
  clipboard/modes/title/pong/ackOnly pass; the batch's window frame plus
  replay carry everything the suppressed rows carried); a
  throttled resume (no ack ever
  arrives) stays pre-ack: no capability, no request, top marker
  neutral; capability read from bit1, not bit0 — `ledgerLost` is not
  paging); data timeout releasing single-flight + `solicitedPending` AND
  setting `budgetCeiling = max(125, ⌊timedOut / 2⌋)` with `effMax` to the
  125 floor; recovery `effMax = min(2 · effMax, budgetCeiling)` per
  CONTAINED reply — a link that carries 500 but not 1000 times out ONCE,
  climbs 125 → 250 → 500 over three replies and HOLDS 500 (the settle
  claim as an executable row; a later timeout at 500 re-converges to 250
  rather than pinning, and no state leaves `effMax` permanently under a
  size the link carries); a
  correlated frame EXTENDING beyond the shrunken retry window applies
  its in-window intersection only — the §5.1 clip — and changes NO
  control state: no release, no growth, the attempt's own contained
  reply does both (a byte-short or empty reply IS contained and DOES
  release);
  the shrunken-retry ANCHOR (at `effMax = 125`, approaching from
  above, the reply lands `[gHi − 125, gHi)` — adjacent to the reader,
  not `pageSize` below the edge);
  the pending-demand timer (fake clock: byte-short continuation and
  data-timeout retry both fire with NO further user scroll;
  alt activation CANCELS the timer, and a clock-fired trigger run while
  alt is active is blocked by the guard);
  the marker gates (pre-ack neutral top marker, `unsupported` legacy
  predicate, `supported` explicit predicate — pinned so the three cannot
  drift) and the gap-idle DEFAULT (`supported`, gap present, nothing in
  flight → the marker asserts a hole, not neutrality, not loading); the
  top-marker predicate (an empty frontier reply raises the floor to
  `oldestHeld` — the top marker turns PERMANENT and does not vanish with
  the emptied pseudo-gap; `serverOldest ≥ oldestHeld` with a low floor —
  the supplied-cap-over-ring steady state — also reads PERMANENT, never
  idle-forever);
  the compatibility tail cap (old-server pairing retains 5000 reachable
  lines with zero requests sent; the cap-flip phase — cap set, then
  WINDOW-FLOORED reclassify (keep `max(supportedTarget, windowHeight)`,
  never crossing `win.base`: a 5000-line store with a 2000-row window
  reclassifies exactly 3000 and zero window rows; `scrollbackLines: 500`
  under a 1000-row window puts no window index in `browse`),
  then ONE WIDER-BAND pass to `prefetchThreshold` when the viewport is
  outside the
  band (a 3500-line band drains 3000, keeping the 500-line scroll-up
  buffer) and to `browseCacheCap` when inside; explicit
  `scrollbackLines` 500 and 10000 hold in every capability state; epoch
  reset reverts to the compatibility cap while socket replacement does
  not; snapshot on both sides of the flip; the TTL clock stamped at the
  flip; the READING-POSITION assertion — viewport parked mid-band at the
  flip stays on screen, its rows reclassified, not evicted);
  the replay jump (PREDICTED inside the transition from the SENT
  `haveThrough`/`replayMax` plus the ack's `serverOldest`/`committed` —
  both causes: the new clamp AND
  the plain eviction gap an old server produces; a pre-ack live frame
  interleaving — the one input that separates `sentHaveThrough` from
  `highest` — must NOT mask the jump: the prediction fires identically
  with and without a dropped pre-ack screen frame; under `unsupported`
  the stranded band stays TAIL — two disjoint runs, today's behavior,
  no browse reclassify, no TTL stamp, no trim markers; under
  `supported` the stranded band —
  OLD window rows included, the descriptor retired — reclassifies before
  ANY batch frame applies, so the batch's window frame cannot
  `enforceCap` it away under the freshly flipped cap: zero band lines in
  `evicted`, `everEvictedThrough` untouched, tail contiguity holds
  afterwards, gap markers render, the band's TTL clock stamped; a
  retired-descriptor store survives the ack-to-window-frame gap without
  evaluating a window-derived bound (no `truncateBelowWindow` while
  `win` is empty); an empty or fresh store predicts vacuously and the
  reclassify is a NO-OP with nothing wired to it; the
  WIDER-BAND budget pass runs once — a supplied `scrollbackLines: 10000` jump
  drains to the target at the ack boundary instead of holding 10000
  browse lines for the TTL, and a jump band CONTAINING the viewport
  keeps `browseCacheCap`, never the flip-local 500; a hydrated 1500-line
  snapshot 20000 indices
  stale attaches with exactly `replayMax` new lines and one healable
  gap; a cold attach at `replayMax = max(1, effectiveTarget −
  windowHeight)` keeps every downloaded row — no immediate `enforceCap`
  trim — and the degenerate tall-window shape (`scrollbackLines: 500`
  under a 1000-row window → `replayMax = 1`) attaches with the window
  plus one line and pages the rest); `solicitedPending`
  single-slot lifecycle; interval merge/split/evict with the viewport
  exemption and EXACT overflow removal (a one-line overflow evicts one
  line; viewport mid-cache at 2500/2501/3500 terminates without
  thrashing); the bulk-apply transient (the browse invariant is
  re-established by the apply's own budget pass; peak ≤ steady +
  `pageSize`, asserted at operation boundaries only); live-apply cannot
  evict browse lines and page-apply cannot
  evict tail lines; `everEvictedThrough` advances on tail trims only;
  classification integrity across `applyScrollbackCleared`,
  `truncateBelowWindow`, and `fromSnapshot`, with property generators
  producing OVERLAPPING fetches (a re-fetch adds no cache rows); the
  retained-range memo following every key-set mutation; snapshot excludes
  browse BY
  CLASSIFICATION (test: a fetched interval flush against the tail);
  decoder rejection of unsafe inbound indices (`MAX_SAFE_INTEGER + 1/+2`
  shapes); anchor preservation across a multi-frame 1000-row prepend;
  gap markers re-derive on low-edge heals and keep `data-abs`
  monotonicity; gap markers render in the pre-ack instant (the
  kept-cache reconnect splice).
- Integration (through the real ui kernel, not renderer units): the
  scroll seam fires the controller (and does NOT fire on the library's
  own prepend writes); `onHistoryReply` routes to `applyHistoryScroll`;
  the `solicitedPending` port clears on socket close; the port's
  consumer-built `applyResumeAck` closure runs the whole ack transition
  (cap flip, jump, wider-band pass) reading the renderer's live
  viewport;
  `historyBudget()` reaches the trigger, so a shrunken retry's anchor
  moves with its length;
  byte-limited partial replies combined WITH the pacing bucket (the
  continuation loop drains THE REQUESTED PAGE WINDOW with no user input,
  via the timer — draining a WIDER gap additionally needs the viewport
  to follow the healing edge, the E2E row's scripted shape, asserted
  there and not here); `dropBrowseCache` visibility gate (hidden page +
  viewport on browse rows → the drop happens; visible + viewport on
  browse rows → the skip re-arms).
- E2E phone shape: 1500 resident, 20000-line ring, scripted scroll to the
  bottomless top, assert full history reachable, residency bounded, a
  mid-history gap (forced eviction) heals on approach, and the §3 byte
  estimates sanity-checked on-device.
- Mutants: threshold off-by-one (prefetch never fires); solicited-set
  bypass (unsolicited accept); TAIL evicted under browse pressure AND
  browse evicted under tail pressure (both directions); byte budget
  removed; row ceiling removed; strip predicate header-blind (a
  262,126-byte row packs and the one-row reply exceeds the budget);
  autolink bit survives the strip; strip made budget-relative instead of
  canonical (the cross-path byte-identity test fails); shrink direction
  inverted; empty-reply
  `firstIndex` forwarded from `LinesFrom`; `ledgerLost` wired as a paging
  trigger (must fail a test; the consumer-level full reset on
  `onServerRestart` is the separate, named behavior in §5.1); eviction
  removes a full page on a one-line
  overflow (thrash mutant); the cap-flip phase evicts instead of
  reclassifying (the reading-position test fails); the flip's
  reclassify crossing the LIVE `win.base` (a live window row lands in
  `browse` — must fail; the §5.2 jump band crossing the OLD
  base after retiring the descriptor is intended behavior pinned by the
  jump row); the budget pass gated on the FLIP alone, so a jump-only ack
  leaves its band unbudgeted (must fail); jump prediction
  taken from `highest` instead of the SENT `haveThrough` (the
  pre-ack-frame interleaving row fails); pre-ack ROWS applied
  instead of suppressed (same row fails); a pre-ack `scrollbackCleared`
  suppressed along with the rows (the client keeps discarded history —
  must fail); the jump reclassify run under
  `unsupported` (the old-server band becomes disposable — must fail);
  recovery without `budgetCeiling` (reset-to-`pageSize` oscillates) and
  recovery capped BELOW `effMax` (growth never fires — both must fail);
  a non-contained correlated frame releasing single-flight
  or growing `effMax` (the late-oversized-reply row fails); the
  client-side `replayMax` clamp removed (the supplied-10000 jump row
  fails: prediction and honored `replayFrom` diverge);
  the replay clamp removed
  (a cold attach to a 20000-line ring
  must fail the ≤ `replayMax` assertion).

## 8. Rollout

1. Engine minor: server control + `LinesRange` + history bucket + the
   resumeAck `historyPaging` bit (declared only at
   `scrollbackCapacity ≥ paginationMinRing`, §4.5) + the bounded replay
   (`ReplayMax *int64`);
   client store/controller/renderer work (`applyHistoryScroll`,
   intervals, gap markers, capability from the ack tail,
   `requestHistory`, the `scroll.ts` `onScrollPosition` seam); the
   capability-driven cap flip (`compatibilityCap` 5000 →
   `supportedTarget` 1500 as the ack transition's cap-flip phase, §5.3).
   Protocol stays
   v4 (capability-declared, not version-gated).
2. ui minor consuming it: NOT api-neutral — it wires `requestHistory` +
   `historyBudget` into
   `render.init`, hands the connection the
   `{ noteSolicited, clearSolicited, applyResumeAck }` store port (the
   `applyResumeAck` member built by the consumer as a
   viewport-capturing closure over the store's one ack transition,
   §5.3/§4.5), routes `onHistoryReply`, forwards the scroll seam,
   wires `getReplayMax` beside the existing `getHaveThrough` callback
   (the resume control is BUILT INSIDE the engine's `connection`; an
   unwired callback means the field is OMITTED, never 0 — §4.5), and
   owns the
   §5.6 visibility hooks + per-tab TTL timers
   (`dropBrowseCache`/`lastBrowseActivityMs`), plus the `scrollbackLines`
   semantics note. Mixed-version strategy is capability-driven, not
   release-ordered: the §5.3 compatibility tail cap makes any pairing
   safe in either deployment order.
3. No per-app capacity call: the depth is the engine's default (§3), so
   an app that wants it does nothing. wtk pins that with a test forbidding
   a `WithScrollbackCapacity` call, so the number cannot drift back into an
   app. An operator overrides per deployment through `WT_SCROLLBACK` — and kiro
   passes NO explicit client-side `scrollbackLines`, so the
   omitted-option semantics govern (an explicit value would pin BOTH
   capability states to it, §5.3, and kiro wants the flip).
4. Decisions this design absorbs from the standing owner list: the
   phone-cap question (§6), the mid-scrollback seam marker (§5.4's marker
   model), background-tab store trimming and viewport-windowed DOM
   (largely moot at 1500 resident — revisit only if jetsam persists), and
   hidden-page DOM shedding (superseded by the §5.6 TTL policy).

## 9. Open questions (owner)

None. The ring depth is the engine's own default (100000) and ships with
the minor; resident default → 1500;
browse-cache lifetime → the §5.6 uniform TTL (all owner-ratified 2026-08).

## 10. Appendix: disposition of the remaining owner-list proposals

2026-08 walkthrough of the client-memory audit's owner list, re-assessed
against this design (all rows owner-ratified). Items the design absorbs
outright — phone cap, seam marker, background-tab trimming,
viewport-windowed DOM, hidden-page shedding — are in §8.

<!-- markdownlint-disable MD013 -->

| # | Proposal | Post-paging assessment | Recommendation |
| --- | --- | --- | --- |
| 1 | WOFF2 conversion (+ optional subsetting) for the four Nerd-Font faces | One typeface, four style faces. Owner condition: adopt only if published at the source. First check (nerd-fonts v3.5.0 release archive) failed — OTF-only. Second check passed one level further upstream: GitHub's own Monaspace repo publishes OFFICIAL nerd-fonts-patched WOFF2 webfonts. Gated on a deterministic metrics check instead of an eyeball: fontTools comparison proved outlines byte-identical and every PUA icon advance exactly one cell (0.62 em, same as letters) across all four faces vs the previously shipped NFM OTFs. 10.9 MB → 5.1 MB served (−53%); download/cache win, not steady-state RAM | ADOPTED + IMPLEMENTED (owner, 2026-08): ui css/tokens/kernel renamed to "Monaspace Neon NF"; kiro + server Dockerfiles fetch four per-face sha256-pinned WOFF2 from githubnext/monaspace v1.400 (Renovate github-releases + repin markers); kiro preload + cache-policy comment updated; dev-build.sh parsers follow the new ARGs. All suites green; fetch layer proven against the live URLs |
| 2 | Trailing default-styled blank trim in row wire encoding | UX-audited 2026-08 vs xterm.js/tmux: their string/copy paths right-trim trailing blanks already, so post-trim copy behavior matches the industry norm — and improves fidelity, since today's grid pads every copied line with spaces the app never printed (a "$ " prompt row ships ~118 selectable trailing spaces over wire + store + DOM), while an INTENDED trailing default-style space is already indistinguishable from padding in any cell grid (xterm.js has the same ambiguity). Mid-line/leading whitespace stays byte-exact; paste INTO the terminal is untouched. Guards: trim ONLY fully-default-style, link-free trailing spaces (erase writes the app's bg into cells, so colored tails are style-carrying and untouchable); never trim a soft-wrapped row (xterm.js #1286's lesson: its tail spaces are mid-line content); a fully-trimmed (empty) row must still render at full line height. One choke point: `cellsToRuns` — whose signature gains the row's wrap flag (today it receives only the cell slice and cannot see wrap state). Subsumes #4 | ADOPTED (owner, 2026-08), folded into this design's engine minor — same wire-touching release, one review cycle |
| 3 | Text-node fast path for default-styled runs | Not free: wide chars + hyperlinks need elements, so this adds a second rendering shape (per-branch "which shape?" tax) through the hottest render code; win is ~0.5–2 KB DOM per plain row, low single-digit MB at 1500 resident | DEFERRED (owner, 2026-08) — revisit only if post-paging device profiles still show DOM pressure |
| 4 | Blank-line interning | SUBSUMED by #2, reasoned 2026-08: after the source-side trim, a fully blank row travels and lands as an EMPTY run list end-to-end (wire → store → DOM), so interning would deduplicate empty arrays — nothing left to share. The unified principle is #2's: stop representing absence at the source rather than caching it downstream. Merged-view addition to #2's checklist: an empty row must still render at full line height (test the blank-line non-collapse) | DROPPED (owner, 2026-08) — superseded by #2 |
| 5 | Remove per-row `data-abs` attribute | ~20–40 B/row, <60 KB at 1500 resident — a rounding error post-paging; the 2026-08 read-anchor fix actively consumes the attribute, the gap markers (§5.4) now consume it too, and it doubles as the free substrate for any future debug/row-identity overlay | DROPPED permanently (owner, 2026-08) |
| 6 | Compositable working-glow | ADOPTED + IMPLEMENTED (owner, 2026-08) after a live A/B demo (web.cplieger.com/glow-demo.html, since removed): eyeball check passed, owner measured 15→30 fps at 1000 dots — the win is battery/heat while an agent works, invisible at one dot. Landed in `web-terminal-ui` css/30-tabs.css: disc static + promoted to its own layer, beat moved to a shared ::before overlay animating OPACITY over a pre-painted bright radial, keyframes now paint-property-free, reduced-motion removes both pseudo layers. Hard-won subpixel lesson (three demo iterations, owner-caught): a compositor layer with a HARD edge at fractional geometry snaps off-centre at DPR 1 whatever positions it — promote the parent so nested layers snap together, and FEATHER the overlay's edge so any residual half-pixel lands inside the feather (the ripple proved the property). Contract test updated + red-checked (paint-property-in-keyframes and overlay-survives-reduced-motion mutants both fail). Rides ui 5.3.0 | DONE |
| 7 | Outbox chunk coalescing + Blob promise-chain FIFO pump | Judged 2026-08 against the owner's bar ("drop unless the optimization is ALSO simply cleaner code"): coalescing replaces the already-simple chunk array with copy logic + thresholds — fails. The Blob pump restructure adds machinery — fails. Noted for future readers: the socket sets `binaryType = "arraybuffer"`, so per spec the Blob branch is dead code and DELETING it would be simpler — but the branch exists against a claimed iOS-Safari deviation, carries two review-finding guards, and a wrong deletion silently bricks rendering on the primary platform; that is a bet, not a cleanup. Do not re-derive | DROPPED (owner, 2026-08) |
| 8a | Engine `dispose()` teardown APIs | No current consumer destroys terminals and keeps running | SKIPPED (owner, 2026-08) — build when a consumer needs it |
| 8b | Hostile-server height bound (client-side clamp) | Owner reasoning, architecturally exact: these servers SERVE the client bundle, so the server is the client's trust root — a hostile server ships a clamp-free bundle, making a client-side clamp against it theater. Only an embedder bundling the ui against a foreign server would benefit; none exists | SKIPPED (owner, 2026-08) |
| 8c | Client alt-gate: accept solicited history while alt is active (race-review residual) | IMPLEMENTED (owner, 2026-08), standalone rather than waiting for paging: `applyScroll`'s alt gate now accepts lines strictly below the frozen main `win.base` (they surface at alt exit's rebuild); at/above-base still drops; a fresh attach straight into an in-alt session (no main window ever seen) accepts everything, matching the main path. Server-side residual comment + steering updated; store tests extended (frozen-base drop, below-base accept, windowless-attach accept) | DONE — closes the write-ordering race's last residual |
| 8d | Resume-spam throttle (per-socket rate limit) | IMPLEMENTED (owner, 2026-08). No rate library exists in the engine (zero-dep policy), so a ~20-line read-loop-owned token bucket on `clientState` (burst 10, one token per 2s — generous anti-amplification, not metering: caps a 1000/s spammer at 30 full resume transactions/min while sitting ~100x above measured legitimate phone cadence). Over-limit resumes drop ackless with a warn; the v4 framing latch still arms. Unit-tested (bucket semantics) + e2e (burst+1 yields exactly burst acks, socket stays healthy), red-checked (gate-removed mutant fails); `resumeControl` extracted from `handleControl` (gocognit) | DONE |

<!-- markdownlint-enable MD013 -->

## 11. Review log

Why the design is shaped the way it is, round by round. Each round was an
adversarial review by two or three independent reviewers; their full reports are
working artifacts kept outside this repository, so what survives here is the
outcome and the reasoning, which is the part a future reader needs.

- impl-r3 (2026-08, greenfield simplification pass before the third
  adversarial round on the IMPLEMENTATION): the client residency model was
  rebuilt on a KEY SET after the previous round's fixes were traced to a
  shared cause — classification held in a structure whose members the store
  need not hold. Structural outcomes:
  - `browseIntervals` + `browseCount` became `browse: Set<number>`. Two
    defect families died with them rather than being patched again: the
    O(span) walks (a hole-spanning band is a hull as wide as a reconnecting
    client is behind — 1.2 s of main-thread work at 80M, now 0.2 ms), and
    the recount arithmetic (rows stored INTO an already-covered range
    counted zero — 3048 held, 1524 counted). `tailCount` and the cache
    count are now read off the containers, so no mutation path maintains a
    number. `addInterval`/`subtractInterval`/`intervalsContain`/
    `intervalsLength` and `widerBand` were deleted with their tests;
    `intervals.ts` is now purely the gap-geometry source.
  - The reclassify primitive takes KEYS, not a range, so no span loop
    exists anywhere in the model. Every caller already knew its keys.
  - ED3 became a lifecycle rather than a line drop: it cancels
    `solicitedPending` and snaps `pagingFloor` even when nothing is held
    below the bound. The §5.5 claim that a stray reply is then "dropped by
    guard 2" was WRONG — the watermark deliberately does not advance on
    ED3, so nothing refused it. A page apply with no in-flight window now
    drops the frame whole, which is the only layer that can distinguish it
    from the legitimate reprint an app emits below its new window base.
  - A ROWS-LESS screen frame no longer redefines the window. The pre-ack
    ED3 forward is exactly that shape, and taking geometry from it put the
    window bottom below its own base, handing `truncateBelowWindow` every
    retained row as stranded: a 3024-line store wiped to 0, silently.
  - The store's cap knob became an OPTIONAL parameter instead of a default
    VALUE: `new LineStore(5000)` was indistinguishable from silence and had
    its cap replaced by the small post-flip target, reachable from any
    consumer passing `scrollbackLines: 5000`. Five call sites lost the
    `x !== undefined ? f(x) : f()` dance the sentinel forced.
  - Retained-range geometry is MEMOIZED (95 us -> 0.9 us per probe at 4500
    rows; the trigger asks per scroll event, the gap markers per flush).
  - Consumer side: the browse-cache TTL sweep now reaches EVERY store, not
    just the bound one (a background tab's cache was immortal for the life
    of the page), and the return-transition drop was DELETED — it ran with
    hidden-page semantics and deleted the rows the returning reader was
    parked on, buying at most 60 s over the periodic sweep.
  - The viewport exemption's "unreachable floor" comment was corrected: it
    IS reachable at the reclassify target, with a measured shape (two runs
    straddling the reader, 800 rows against a 500 target).

  Then the third adversarial round on the implementation (two reviewers)
  found 3 highs across them, two of them reached independently by both:
  - ED3 was handled INSIDE the main-screen branch, so an alt-active frame
    skipped the entire lifecycle (measured: 300 rows retained, floor 0,
    window still open, and a reply then resurrecting 100 erased rows).
  - `paginationMinRing` (5000) and `maxReplayLines` (2000) were INDEPENDENT,
    so a ring configured in 2001..4999 replayed its newest 2000 lines with
    the paging bit clear and stranded the rest in the authoritative ring — a
    defect the unconditional replay bound introduced by retiring the premise
    the 5000 was chosen under. The threshold is now DERIVED, with a test on
    the invariant rather than on the values.
  - A page reply whose data timeout had already released single-flight came
    back UNCORRELATED, through the ordinary scroll path, classified TAIL —
    outside the cache budget and the TTL both, so it never left. Closed by
    the erased watermark above. An earlier attempt used `pagingFloor` for
    this and was reverted: it also refused the app's legitimate reprint.
  - §5.4's three-state top marker was specified but never implemented: the
    renderer showed no marker for a fetchable frontier, or labelled it
    "trimmed". Implemented, and the spec's "pre-ack neutral" clause
    corrected to the fallback the code actually needs.

  Two round-3 items were first reported as accepted residuals and then, on a
  second greenfield pass, fixed at the root instead:
  - The containment decision now takes `following` from the RENDERER rather
    than deriving it. Both derivations had been wrong: from the window
    descriptor (which step 4 retires — r2's high, patched by sampling
    earlier, hazard intact) and from cache membership (which drains a reader
    whose anchor row is not held — 801 rows instead of 2420, reachable from
    an ordinary armed restore). Deleting `isFollowing` removes the class;
    the store cannot answer the question and no longer tries.
  - The frozen-page residual is closed by a last-chance hook (`freeze`, and
    `pagehide` with `persisted` for Safari's bfcache) that drops every cache
    unconditionally. Unconditional is right as the page STOPS running and
    wrong as a reader arrives, which is the distinction that justified
    deleting the earlier visibilitychange drop and adding this one.

- r8 (2026-08, two reviewers; the third did not complete its run):
  claude 1H/3M/8m, gpt 1H/3M/2m, and the two converged on the same high
  and on two of the three mediums. Structural outcomes:
  - The `maxReplayLines` clamp moved to BOTH SIDES (client clamps before
    sending, server clamps what it honors), because a server-only clamp
    silently changed a number the CLIENT predicts with: both reviewers
    independently walked a supplied `scrollbackLines: 10000` attach where
    the prediction used the sent 9960 while the server replayed from
    `committed − 2000`, leaving a real jump undetected and `enforceCap`
    evicting the stranded band — r7-H1n's failure class with a
    server-side cause, at exactly the configuration the clamp was added
    for. `replayMax` now has ONE meaning document-wide (the clamped
    value) and `maxReplayLines` is named a shared protocol constant.
  - The adaptive budget was rebuilt on RFC 5681's actual shape after
    both reviewers proved the capped-doubling rule could never fire:
    after a halving the remembered size is exactly `2 · effMax`, so
    "double while below half the remembered size" reduces to
    `2e < e` — `effMax` was monotone non-increasing for the socket's
    life, §7's growth row could only observe no change, and two
    transient timeouts pinned a 500-capable link at 250 forever. Now a
    timeout sets `budgetCeiling = max(125, ⌊timedOut / 2⌋)` and drops
    `effMax` to the 125 FLOOR; each contained reply grows
    `effMax = min(2 · effMax, budgetCeiling)`. The ceiling is a growth
    TARGET, so recovery converges on the largest working size FROM
    BELOW (125 → 250 → 500, then holds) and a later degradation
    re-converges instead of pinning.
  - The pre-ack rule became FIELD-AWARE rather than a blanket frame
    drop (gpt, with claude concurring): a screen frame is not only rows,
    and `scrollbackCleared` is a consumed one-shot the resume batch
    hard-codes false — dropping it wholesale would leave the client
    displaying history the server had already discarded. Row and window
    mutation are suppressed; ED3 routes to the store's clear path; the
    piggybacked `inputAck` applies; `bell` is dropped by design;
    clipboard/modes/title/pong/ackOnly pass. Also recorded: a pre-ack
    frame is the DESIGNED first delivery on a busy session (the accept
    path marks dirty), and the rule's safety rests on "an ack always
    follows", which holds for this client because it sends one resume
    per socket against a bucket that starts full.
  - `applyResumeAck`'s bounds became NULLABLE AS A PAIR (gpt: the ack's
    bounds live in its own length-gated tail, so 9- and 17-byte acks
    carry none — the transition must still reset on an epoch change and
    still read `unsupported`, and must never substitute zero bounds,
    which would lower `pagingFloor` and forge a prediction). Its CALL
    SITE is now named: it SUBSUMES the shipped `onResumeBounds`
    callback, and the consumer's epoch `onServerRestart` reset must stay
    ahead of it (claude: a reset landing after would revert the cap
    under a `supported` connection).
  - The budget pass's band is the WIDER of the two, not a "union"
    (claude: the bands are nested by construction, so union machinery is
    unreachable), and CONTAINMENT is now defined: a FOLLOWING viewport
    is outside every reclassified band, and a scrolled-up viewport is
    inside iff it sits on a row the pass just reclassified (membership
    in the reclassified intervals, not the band's numeric hull — a
    viewport parked in a hole protects nothing). §3's memory picture was
    re-keyed on that discriminator and gained the recurring
    stale-reconnect shape (~2000, previously unlisted).
  - The `unsupported` promise was scoped (claude: "no trim markers, two
    disjoint runs" is false against a DEEP-RING old server, which does
    not honor `replayMax` and can replay past the compatibility cap —
    `enforceCap` then evicts the band as ordinary tail and reconverges
    to one run with today's marker). The claim is now "no worse than
    today" rather than "lossless", §5.2's model sentence carries the
    two-run exception, and gap markers are pinned to read the retained
    KEY SET rather than cache membership — the only shape where the two
    differ is that two-run tail, and reading the wrong one would splice
    unrelated regions with no marker.
  - Minor-tier: the advisory-decode premise was corrected (Go skips the
    malformed field and completes, returning an error; the handler's
    blanket error return is what drops the control — and `HaveThrough`
    carries the same shipped exposure, left as-is deliberately); the
    replay throughput figures were labelled by domain (~150 KB default,
    ~200 KB at the server maximum); and two pre-rename phrases from r7
    were cleaned up.
- r7 (2026-08, all three reviewers): claude
  2H/6M/7m, gpt 0H/3M/5m, fable 0H/1M/1m — convergent on three seams in
  the v7-new material; fable's executable round proved the four-step ack
  transition closes the r6-H1 shape (18/18, with a control run
  reproducing the v6 defect) and independently measured the doubling
  defect. Structural outcomes:
  - The jump prediction switched to the SENT inputs
    (`sentHaveThrough`/`sentReplayMax`, captured by `connection` when it
    builds the resume control) and PRE-ACK content frames are now
    DROPPED (claude + gpt convergent: registration precedes resume, so
    a flush can deliver a live frame to a not-yet-acked socket — it
    advances `highest`, masking the jump predicate built on it, and on
    an already-flipped store it runs `enforceCap` under the stale cap
    before the transition exists; prediction must use what the server's
    reply is a function of, and the drop is lossless by supersession —
    the batch's window frame and replay carry everything a pre-ack
    frame did).
  - The jump reclassify was CAPABILITY-GATED (claude: an old server's
    ordinary eviction-gap resume would have converted the retained tail
    into disposable browse cache — drained to 500, TTL'd to zero,
    permanent trim markers, no paging to compensate: worse than the
    regression the compatibility cap exists to prevent, and up to 7500
    resident lines under `unsupported`). Under `unsupported` the
    stranded band stays TAIL — today's behavior exactly — and §7's
    generated tail-contiguity property is scoped to `supported` (the
    shipped store already violates contiguity benignly on that shape).
  - The five ack steps became ONE STORE TRANSITION
    (`applyResumeAck({...})`, wrapped in a single consumer-built
    closure; the port's third member replaces `confirmPaging`, which is
    now the transition's cap-flip phase) after claude showed the four
    steps spanned three layers with a seam for only one of them — the
    r5 zero-arg-closure defect's shape one layer up. The budget pass is
    owned by the transition's final step and runs over the wider of the
    flip and jump bands (claude: the flip-local reading would have
    drained 3000 lines at the 500 target before the jump step defined
    the band a parked viewport was inside).
  - Doubling recovery gained its `ssthresh` analog: a contained
    correlated reply doubles `effMax` only while the doubled value
    stays below HALF the remembered timed-out size (claude + fable
    independently: plain doubling re-probed the failing size — 500 × 2
    = `pageSize` — so the link the rule exists for still timed out on
    every other request, the v6 sawtooth verbatim; fable measured it).
    The name AIMD was dropped as wrong (gpt + claude: doubling is
    slow-start-shaped, not additive increase). CONTAINMENT now gates
    control effects (gpt: a timed-out 1000-line reply sharing the
    retry's `fromAbs` correlates by `firstIndex` alone; it applies its
    clipped intersection but must not release single-flight or grow
    `effMax` — the attempt's own reply does both).
  - `effMax` got its missing channel and lifecycle: a consumer-wired
    `historyBudget()` accessor beside `requestHistory` (claude: the
    §5.4 trigger lives in `render.ts` with no path to connection-owned
    state — the anchor arithmetic was uncomputable as specified), and
    membership in §4.4's atomic per-socket reset list (a reconnect is
    usually a link change).
  - `replayMax` gained an upper clamp (`maxReplayLines` 2000 —
    resident-tail order, deliberately not ring depth; claude: "the ring
    clamp bounds them" was a correctness claim standing in for a byte
    claim, and a supplied `scrollbackLines: 10000` would have replayed
    ~1 MB inside the close-on-expiry 10 s context, r5-H4's bootstrap
    loop at a legal configuration) and an ADVISORY-safe decode (gpt +
    fable: a plain `*int64` unmarshal errors on a malformed value and
    the shipped handler would drop the whole resume — malformed now
    reads as absent).
  - The top-marker predicate gained the `serverOldest ≥ oldestHeld`
    disjunct (claude: the trigger's own frontier stop had no marker
    mirror, so a supplied cap over a shallower ring — a steady state
    `paginationMinRing` newly admits — would have shown
    "not loaded" forever about content that can never load).
  - Minor-tier: the replay count got its outer `max(0, …)`
    (`haveThrough` can legally sit at or above `committed` — gpt); the
    §5.2 retirement's three consequences were stated (vacuous alt gate
    during the batch, no window-derived bound evaluated while retired —
    `truncateBelowWindow` would read −1 and delete the store — and a
    one-frame cursor-overlay artifact between message tasks — claude);
    the tall-window degenerate (`replayMax` collapsing to 1) and the
    empty-store vacuous prediction were named (claude); `replayMax`
    spelling unified to `max(1, effectiveTarget − windowHeight)` with
    the prediction's term conditioned on the field having been SENT
    (claude); the 4999/5000 `paginationMinRing` boundary pair joined §7
    against the constant (claude); and `effMax` joined §5.5's
    close/replace cleanup list (gpt).
- r6 (2026-08, all three reviewers, the third back after its incomplete r5
  attempts): claude 2H/6M/7m, gpt 0H/3M/4m,
  fable 0H/0M/2m — fable's seat MET the bar, and its executable round
  validated the ack-tail bit against the real encoder and decoder (13/13
  shapes incl. truncated tails and bit0/bit1 independence), the replay
  clamp against the real 20000-line ring, the flip-drain arithmetic
  (residency 2000, mid-band reader survives), the halving × pacing × 8 s
  composition (zero server-bucket drops across 200 jittered trials), and
  every number v6 touched, several to the byte. Structural outcomes:
  - The replay-jump reclassify moved from "before the first replay line"
    to a FOURTH ACK STEP with a prediction formula (claude: the resume
    batch writes the WINDOW frame before the replay, so each of its
    `applyLine` calls would have run `enforceCap` against the
    still-tail-classified band under the freshly flipped cap — deleting
    ~129 lines and advancing `everEvictedThrough`, the two things the
    jump paragraph promises cannot happen). The condition also
    generalized from "the clamp" to ANY replay landing above
    `highest + 1` (the plain eviction gap old servers produce today
    creates the same shape, and §7's generated tail-contiguity property
    would have failed on it), and `noteResumeBounds` now keeps
    `committed` (the prediction's input; the current store discards it).
  - The capability bit gained a RING-DEPTH condition
    (`scrollbackCapacity ≥ paginationMinRing`, 5000 — claude: the
    engine's default ring is 1000, so a default-configured NEW server
    would have flipped the client to 1500, returned empty for every
    page, and painted permanent trim markers over history the client
    held a minute earlier — r3's 5000 → 1500 regression reached through
    configuration instead of server age; the bound is client-side
    ACCUMULATION, which is why the threshold is the legacy client
    default, not the resident target).
  - The jump band now INCLUDES the old window rows with the window
    descriptor retired first (claude: `highest` is the old window
    bottom, so the band's top slice is window rows that the shared
    primitive's `win.base` guard would have refused — leaving them as
    the tail's oldest keys, first victims of the next `enforceCap`), and
    the jump ends in the SAME viewport-aware budget pass as the flip
    (claude: with a supplied `scrollbackLines: 10000`, a jump would have
    held 10000 browse lines against the 2500 invariant for up to five
    minutes, and §7's own generated property would have failed).
  - `replayMax` got its wire contract (claude + gpt + fable converged):
    `ReplayMax *int64` — ABSENT means full replay (a scalar field's zero
    would have deleted the replay for every OLD client against a new
    server); invalid values are treated as absent, never a dropped
    control; the server honors it ONLY when it declares bit1 (an
    optimistically sent field must not shrink an unsupported pairing's
    backfill); the client sends `supportedTarget − windowHeight` (a cold
    attach no longer downloads ~129 rows for `enforceCap` to trim); and
    the 10 s-context claim was scoped to typical content (gpt: the legal
    adversarial extremes — ~393 MB of maximal replay rows — stay §4.2
    residuals; "comfortably inside 10 s" was false as an absolute).
  - The adaptive budget became AIMD and got a name (`effMax`): it now
    feeds BOTH the trigger's anchor and its length (claude + gpt: a
    125-line retry anchored at `gHi − pageSize` healed 875 lines away
    from the reader — the exact failure approach-anchoring exists to
    prevent), and a correlated reply DOUBLES it toward `pageSize`
    instead of resetting (claude + fable: reset-on-success made a
    timeout on every other request the steady state on a link that
    carries 500 but not 1000). The solicited-INTERSECTION clip became
    normative in §5.1 (gpt: a late 1000-line reply can correlate against
    a shrunken 500-line retry window by `firstIndex` alone; only the
    intersection is solicited).
  - Capability processing was ordered BEFORE the ack handler's
    ledger-loss and forgotten-session early returns (claude: the
    ledger-lost attach is the long-absence one that carries a jump;
    appending capability to the branch would have skipped it exactly
    there), and §5.1's "`ledgerLost` is not a paging trigger" was
    scoped to the ENGINE with the shipped consumer's wholesale
    `onServerRestart` reset named beside it (the sentence was false as
    product behavior).
  - Minor-tier: the replay interval went half-open with the exact count
    (`committed` is one past the newest; gpt + claude independently);
    "FIRST frame the server writes" became "first frame of the resume
    batch" (gpt: an unresolved socket can receive a live frame earlier);
    the store API list regained `confirmPaging(viewportAbs)` (gpt: the
    zero-arg spelling invited the r5 seam bug back); the flip's removals
    were re-labelled synchronous STORE removals drained by the renderer
    (gpt: `LineStore` is pure); the budget-pass pseudocode went
    target-agnostic with the termination note split by target (claude);
    `replayMax` moved to a `getReplayMax` consumer callback in §8
    (claude: the resume control is built inside the engine);
    a throttled resume (no ack ever) joined §4.5's outcome table
    (claude); §3's memory picture gained its second exception (viewport
    parked inside the band → 4000, claude); and the disconnected-ED3
    case was documented as one wasted fetch (the resume window frame
    hard-codes `scrollbackCleared = false`; the clamp repair covers it —
    claude).
- r5 (2026-08, two reviewers; the third did not complete its run):
  claude 4H/5M/6m, gpt 0H/3M/2m,
  heavily convergent on the r4 additions. Structural outcomes:
  - The capability PROBE was deleted outright, replaced by a DECLARED
    bit: ackFlags bit1 (`historyPaging`) in the resumeAck's existing
    length-gated tail. gpt proved the probe's 4 s windows measured the
    resume batch's DRAIN, not an RTT (the ack is the first write of one
    serialized batch holding `writeMu` on the read loop; browser
    `send()` only enqueues), so a slow link mis-latched `unsupported`
    silently and permanently; claude proved the probe timeout released
    neither single-flight nor `solicitedPending` — the retry-armed
    re-probe edge could never execute — and that a probe reply landing
    after a tail trim left a persistent one-line island below the tail.
    One flag bit kills the entire machine: no request, no timers, no
    retry states, no mis-latch, and capability truly one RTT after
    attach.
  - The resume replay became BOUNDED (`replayMax: supportedTarget` in
    the resume control; a new server clamps the replay's START). claude
    refuted the ratified "the phone never pays for the ring": a cold or
    long-absent attach shipped the WHOLE ring (~2 MB at 20000 lines by
    §3's own estimate) inside one close-on-expiry 10 s write context —
    a bootstrap loop below ~200 KB/s, quadrupled by the ring raise
    itself — while the design's own `LinesRange`/interval machinery was
    the fix it declined to use. A replay landing above `highest + 1`
    now reclassifies the stranded band as browse (the same primitive as
    the cap flip) and the gap heals on approach.
  - `confirmPaging` gained a WINDOW FLOOR (keep
    `max(supportedTarget, windowHeight)`, never crossing `win.base` —
    both reviewers independently showed the literal "newest
    `supportedTarget` lines" rule reclassifying live-window rows), a
    VIEWPORT-bearing seam (the port member is a consumer-built closure
    capturing the renderer's viewport getter — gpt showed the pass's
    far-edge selection had no specified input from store-blind
    `connection`), and a post-flip DRAIN target of `prefetchThreshold`
    when the viewport is outside the band (claude proved post-flip
    residency was 4000, not the claimed 1500: the pass freed 1000 of
    3500 and the TTL had no defined baseline for a band nobody browsed
    — the reclassify now stamps the TTL clock at creation).
  - The top-of-store marker got an EXPLICIT predicate (permanent when
    `pagingFloor >= oldestHeld`; the frontier pseudo-gap is exempt from
    projection removal — claude showed the old rules deleting the
    marker at the exact moment the ring was exhausted, or the legacy
    false-positive predicate taking over, reader's choice).
  - The 26 KB/s link-floor justification was INVERTED and corrected: it
    sits ABOVE the live path's ~20 KB/s bar at the doc's own byte
    figures, not "far below" it. The honest statement is parity, plus
    ADAPTATION the live path lacks: `maxLines` halves per consecutive
    data timeout (125 floor, reset on any correlated reply) — the r4-m6
    mitigation, declined in v5, now adopted.
  - Minor-tier corrections: the aggregate residual list gained SCREEN
    frames (~250 MB worst legal, same shape as live — both reviewers);
    the flip's "two loop iterations" became `pageSize`-sized removals
    with exact counts; the frontier pseudo-gap's empty/inverted
    geometry got its guards-before-arithmetic clause; the `browseCount`
    "exhaustive" mutation list gained the reclassify primitive; the
    partially condemned gap's marker staging was stated; and the §7
    pre-strip figure was pinned to the `maxOSCLen − 3` expression
    instead of a literal.
- r4 (2026-08, same three reviewers): 3 highs +
  10 mediums + 12 minors, concentrated on the r3 additions (the
  compatibility cap and the row ceiling); the architecture survived its
  fourth round unchanged. Structural outcomes:
  - The capability probe became EAGER — dispatched by the capability
    machine post-resume at `[oldestHeld, oldestHeld + 1)`, a committed
    line the client retains — after the scroll-gated probe was traced to
    a latch only a ~4500-line scroll could satisfy (the never-scrolled
    majority would have kept 5000 resident forever; §3's headline
    common-case number was false as written). SUPERSEDED in r5: the
    probe is gone entirely; the resumeAck bit is the capability signal.
  - The cap flip was rebuilt as `confirmPaging()` on a mutable
    `effectiveTailCap` with RECLASSIFY-AS-BROWSE mechanics: two
    reviewers independently proved the one-time synchronous trim fired
    under the reader by construction (the old latch geometry required
    the viewport at the frontier), evicting ~3500 lines beneath the read
    anchor in one unchunked pass and re-fetching them over the network.
    Reclassification deletes nothing; the far-edge viewport-exempt
    browse pass evicts far from the reader.
  - Explicit `scrollbackLines` semantics were pinned (supplied N holds
    in every capability state; omitted means 5000-compat/1500-target;
    epoch reset reverts, socket replacement does not), and the port
    gained `confirmPaging` (the latch previously had no path to the cap
    it changes).
  - The write context dropped 20 s → 10 s with the client data timeout
    at 8 s, after the blast radius was re-derived: a page reply holds
    `writeMu` inline on the read loop while `dispatchFrame`'s fan-out
    `wg.Wait`s, so one slow phone stalled live output to EVERY client of
    the session — plus its own Ctrl-C — for the full window; v4's
    "self-inflicted and bounded" was false. 10 s matches the accepted
    resume-batch constant. (The "paging floor sits below the live bar"
    justification v5 attached here was itself inverted — corrected in
    r5.)
  - The byte contract became MESSAGE-inclusive (`pageByteBudget − 19`
    header accounting, one `encodedRowSize` helper for strip + packing +
    tests, the 262,125/262,126 boundary pair); the aggregate live/replay
    exposure was RE-SCOPED as open (the row ceiling bounds rows, not the
    1000/50-row message shapes — ~16x better post-ceiling, honestly out
    of scope); the ceiling extended to SCREEN frames (same shared
    encoder; display and history now agree on a stripped row).
  - Strip semantics hardened: pure function of the canonical committed
    row; autolink bit 1024 cleared (a stripped run is a never-linked
    one); client re-linkification named as accepted prefix-href
    degradation; the §4 immutability claim scoped to COMMITTED indices
    below the window; the two load-bearing premises stated
    (`maxResizeCols`, one rune per cell).
  - The server history bucket was re-scoped to per-socket fairness with
    the N-socket residual documented as pre-existing class (a per-socket
    bucket is not an aggregate abuse bound).
  - Gap markers gained their named IDLE state ("earlier output not
    loaded", the DEFAULT in every capability state — the unspecified
    state was the common one).
  - The pending-demand timer gained the alt guard and cancellation (a
    clock fire during a vim session would fetch invisible pages and
    re-arm for its whole duration).
  - The TTL viewport skip became VISIBILITY-gated (a hidden page has no
    reader; the unconditioned skip retained exactly the deep-scrolled
    cache the hidden-page TTL exists to free).
  - The bulk-apply transient was stated (+`pageSize` between apply and
    pass, store peak 5000 = today's cap for microseconds, invariants
    asserted at operation boundaries only), and the eviction-loop
    termination proof was inlined (a contiguous exemption band means the
    farthest interval holding any non-exempt line always yields).
  - The interior-gap floor-raise wording was tightened (condemns
    `[fromAbs, end)` only, not the whole gap), and the r3 log's "~65 MB"
    figure was corrected to ~4 MB through the vt layer
    (`maxOSCLen = 4096` caps every stored URI; all three reviewers
    converged on the number, one by measuring the real encoder —
    4,112,021 bytes pre-strip, 22,021 stripped, the analytic
    22,002 + 19 header exactly).
  - fable's executable round also validated the exact-overflow eviction
    on 2000 randomized shapes, the paced 20-page drain at 32.1 s with
    zero server-side drops, and every §3 number the revision touched.
- r3 (2026-08, same three reviewers): 4 highs +
  18 mediums, concentrated on the r2 rewrite's operational seams; the
  architecture survived unchanged. Structural outcomes: the write-context
  ordering was inverted to client-acts-first (20 s server / 15 s client —
  r2's "server strictly below client" made the data-timeout retry
  unreachable, since a coder/websocket write expiry closes the socket);
  the probe generalized to "one line adjacent to whichever edge
  triggered" (a reconnect keeping a gapped cache made the frontier-only
  probe rule incoherent), with timeout-not-empty as the unsupported
  signal; `enforceCap` gained its third amendment (cursor starts at the
  global `this.oldest` with the hop predicate extended to browse
  intervals — starting at the tail's oldest key would have corrupted
  `oldestIndex()` on the first trim of any browse episode, mis-computing
  the frontier and probe; executable-verified); the min-one-line rule was
  replaced by the per-row URI-strip ceiling shared by ALL scroll paths
  (a legal 1000-run OSC-8 row encodes to ~4 MB through the vt layer —
  the wire format alone would admit ~65 MB, but `maxOSCLen` caps every
  stored URI at 4096; figure corrected in r4 — text+style is bounded
  at ~22 KB, so stripping URIs makes the byte budget real instead of
  aspirational and closes the pre-existing per-ROW live/replay
  exposure);
  browse eviction gained the exact-overflow removal rule + the
  `2·prefetchThreshold + 1 < browseCacheCap` static invariant + a
  progress rule (up-to-a-page eviction thrashes at the cap; an
  all-exempt cache must not spin); pacing gained the coalesced
  pending-demand timer (a dropped continuation on an idle session had no
  future event to retry it — the stall was deterministic); bucket
  identity was pinned to the concrete socket with server-side slack
  (client 2 s / server 1.5 s refill — identical constants plus clock
  jitter let a healthy client trip the silent server drop);
  `pagingFloor` moved into `LineStore` with the epoch-reset-before-bounds
  order pinned; the marker model was decoupled from capability (gap
  markers render whenever gaps exist — a kept cache across reconnect
  would otherwise splice unrelated regions silently; only marker STATES
  are capability-gated, and `unknown` shows neutral, not the legacy
  predicate, which would false-positive within seconds at a 1500 tail);
  the compatibility tail cap was added (an old-server pairing under the
  flipped default would have silently cut reachable history 5000 → 1500
  with no paging to compensate — the flip is now the post-probe steady
  state); the trigger predicate was fixed at floor equality
  (`gapHigh > pagingFloor` — the reopened frontier's low edge EQUALS the
  floor in the steady state, and the strict-gapLow reading would have
  forfeited all re-trimmed history for the session); the §4.1 guard was
  rewritten in the subtraction form (the addition form is itself the
  int64 overflow it rejects); client-side safe-integer enforcement
  became end-to-end (send-side isSafeInteger + decoder rejection of
  unsafe inbound indices); the shrink direction was documented as
  coupled to the clamp encoding (prefix-only; suffix-shrink forges a
  clamp); marker `data-abs` re-derivation on low-edge heals; the
  `solicitedPending` port bridged store-blind `connection.ts`; TTL drops
  skip-and-re-arm when the viewport sits on browse rows; the scroll seam
  fires after `preserveFollowOnce` (a prepend must not re-trigger);
  counter maintenance was enumerated across all five store mutation
  paths with recount-not-increment (overlapping fetches double-count);
  plus the fable-verified boundary-shape executions and a dozen smaller
  wording/test-list corrections. fable's executable round confirmed the
  r2 intersection arithmetic, approach-anchored healing on all gap
  shapes, the write-ctx downlink arithmetic (of the then-current
  constants, since re-tuned — see r4), and the accuracy of this review
  log's r2 entry.
- r2 (2026-08, same three reviewers): 8 highs
  across the three, largely convergent on the r1 rewrite's fresh normative
  material. Structural outcomes: serving became the strict INTERSECTION of
  request window and retained range with uint64 guards (three reviewers
  independently broke the length-preserving clamp — replies could leave
  the correlation window, wedge single-flight, and latch a false
  `unsupported`; r1's own clamp recommendation was wrong); the probe moved
  adjacent to the frontier (`[oldestHeld − 1, oldestHeld)`), where every
  honest answer is correlatable and an empty reply is exact; the
  correlation section gained the content-safety argument (mis-correlation
  is harmless by index immutability) and corrected the live-frame
  `firstIndex` claim (they carry the previous window BASE, not
  `> haveThrough`); the oversized-row/empty-reply ambiguity was resolved
  by the minimum-one-line rule (an empty reply keeps exactly one meaning);
  enforceCap reuse was respecified as partitioned tail/browse accounting
  with numeric invariants (live output could previously eat the browse
  cache at 93 lines per applied line); browse eviction became
  slice-granular with a viewport exemption (whole-interval eviction could
  blank the row being read); the trigger gained the `scroll.ts`
  `onScrollPosition` seam (the existing callback fires only on follow
  TOGGLE — paging would never have fired on an idle session) and
  approach-anchored gap arithmetic (both prior formulas fetched the wrong
  end of wide gaps and escaped narrow ones); the server write context was
  raised to 10 s below the client's 15 s data timeout (a coder/websocket
  write-context expiry CLOSES the socket, so the 5 s draft killed the
  retry path on slow links); the client-side pacing mirror made the server
  bucket unreachable for healthy clients; `pagingFloor` was named with a
  full lifecycle (ED3 snap, resumeAck lowering); the 30 s late-reply grace
  was deleted (it contradicted the single `solicitedPending` slot);
  snapshot exclusion switched from contiguity to classification (a page
  flush against the tail would have serialized); gap markers became
  per-gap `data-abs`-carrying elements (the renderer draws no interior
  holes today, and the singleton marker would have broken anchor
  monotonicity); the `unsupported` marker fallback keeps today's trimmed
  indication for old servers. Style ruling: em dashes stay (repo prose
  convention; the no-em-dash rule governs agent chat output, not
  documents).
- r1 (2026-08, three adversarial reviewers): 8 highs. Structural outcomes: the
  contiguous-block browse model was replaced by the interval-set model
  (§5.2/§5.3) after two reviewers independently proved directional
  eviction re-created the R3 interior-hole bug; reply correlation and the
  empty-reply encoding rule became normative (§4.3) after the empty reply
  was shown to carry `committed` as `firstIndex`; the history token bucket
  (§4.4) replaced the vacuous single-flight abuse claim; the capability
  probe was separated from the data deadline (§4.5); the two-budget
  residency model was mapped onto the store's single-cap mechanism (§5.3);
  fetch-controller ownership moved to `render.ts` with named seams (§5.4);
  `ledgerLost` was removed as a paging trigger; snapshot now excludes the
  browse cache; TTL enforcement moved to `visibilitychange`-primary; plus
  arithmetic/sourcing corrections.
