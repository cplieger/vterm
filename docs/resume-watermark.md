# Resume watermark: excluding the live window from `haveThrough`

Status: proposed, revised 2026-08-21 after three adversarial reviews. Scope:
`web/src` plus one consumer wiring line. No wire change, no Go change in this
change-set; see §2 for a second defect that does need one.

## 1. The harm

A tab switch leaves a frozen copy of the kiro-cli composer box parked in scrollback
directly above the live viewport. Reported from `web-terminal-kiro`, reproducible on
every switch away from and back to a tab that produced output while backgrounded.
Independent of `chat.preserveScrollback` (asserted `false` since 2026-08-21, verified
in the live container) and independent of any resize.

Mechanism, in the client's own terms:

1. A live window row is written at `base + y` into the same map as committed
   history (`applyLine`, store.ts:1487, from `applyScreen`, store.ts:601-606).
2. `highestIndex()` therefore reports the window's BOTTOM row, and
   `truncateBelowWindow` (store.ts:1607) actively maintains that identity.
3. The resume `haveThrough` is that value: `render.getHighestIndex()`
   (render.ts:594) wired to `Callbacks.getHaveThrough` (ui `kernel.ts:604`), sent
   at connection.ts:1284.
4. The server replays strictly above it: `replayStart` returns
   `haveThrough + 1` (terminal.go:2399).
5. A tab switch detaches the socket (`setSession` -> `teardown(); connect()`).
   When this browser is the only viewer the session reaches zero attached clients
   and `retainSuspendedScrollback` (terminal.go:1468-1476) keeps draining into the
   ring; with another viewer attached the ordinary `buildFrame` path appends the
   same rows at terminal.go:1530-1531. Zero clients is the common case, not a
   premise: either way the server's `committed` advances and the client sees none
   of it.
6. On switch-back the client sends its stale `haveThrough`, the replay starts
   above it, and every cached row at or below it is never corrected. Those rows
   are now below the new window base, so the renderer draws them as scrollback.

The row positions the composer occupies are the bottom of the screen, which is
exactly `haveThrough` and the rows just under it. That is why the artifact is
always the input box and always immediately above the live region.

## 2. Two independent causes, and this change fixes one

The watermark above is necessary and NOT sufficient. A second defect produces the
same artifact through the server's ring, so replaying the ring cannot heal it:

- kiro-cli's resize redraw writes `CLEAR_ALL` = `\x1b[3J\x1b[2J\x1b[H`, so ED3
  arrives BEFORE the repaint.
- `handlePTYData` consumes it immediately: `scrollback.Clear()` plus
  `scrollbackClearedPending = true` (terminal.go:1402-1408).
- The repaint's own overflow drains afterwards and is appended to the ring AFTER
  that clear (`buildFrame`, terminal.go:1530-1531), and the clear flag is attached
  to the same frame (terminal.go:1533).
- `dispatchFrame` writes payloads `modes, title, clipboard, screen, scroll`
  (terminal.go:1724-1732) and the clear bit rides the SCREEN message, so the
  client drops history and then immediately re-seats the redraw's overflow.

`armRedrawSettle` makes this the normal path rather than a race: it holds flushes
until the child's redraw output goes quiet, so the ED3 and the whole overflow are
coalesced into one frame. The result is a stale frame tail committed to the ring
as canonical history. That is a Go-side ordering defect, it is out of scope here,
and it needs its own change plus a decision on whether a resize redraw is an
atomic old-for-new transaction stronger than raw xterm ED3 semantics.

So the acceptance criterion for THIS change is "the client stops presenting its
own unconfirmed rows as history", not "the frozen box is gone".

## 3. This is a known, deferred residual

`docs/paged-scrollback.md:778-782` states it:

> A pre-existing nuance, unchanged by this design: `haveThrough` is the old window
> BOTTOM, so a row that changed while disconnected and then scrolled into history
> is not re-sent by replay. Paging neither creates nor fixes that.

Read in context that sentence is an attribution disclaimer, and the weighed
deferral of the same residual lives in the consumer, in `web-terminal-ui`'s
`scrollback.ts` persistence-path comment, which reasons about it as a spinner or a
progress line. Two things changed since. Demand-paged scrollback (2026-08-11,
`91414f4` / `383015c`, ui `3ac6f70`) raised client retention and added a browse
cache whose post-flip drain keeps the NEWEST reclassified rows, which is where
these stale rows sit; before it the shallow tail cap evicted them within seconds.
And the affected surface turned out to be a bordered multi-row box redrawn in
place on every keystroke, not a single progress line.

## 4. Withdrawn framing, recorded so it is not re-derived

The first pass read the store's window handling as ten compensations for a missing
distinction and proposed removing them. That reading is wrong. Each mechanism was
introduced for a measured defect:

| Mechanism | Introduced | Defect it removed | How found |
| --- | --- | --- | --- |
| `truncateBelowWindow` | `4483757` 2026-07-02 | iOS soft-keyboard shrink left the taller screen's ex-bottom rows as phantom scrollable blanks | live use |
| rows-less `applyScreen` return | `91414f4` | a `height = 0` frame put the window bottom at `base - 1` and wiped a 3024-line store, silently | measured |
| guard 2 / 2b `inWindow` exemptions | `91414f4` | a base-RETREAT frame dropped window rows forever; a hydrated store refused every window row and went permanently blank | interleaved property test |
| `tailBound` window floor | `91414f4` | scroll bursts above a stale window escaped the cap (20k lines retained at cap 64, store.test.ts:271) | R2 review |
| `confirmPaging` `winFloor` | `91414f4` | the literal newest-N rule reclassified live window rows into an evictable cache | both reviewers, independently |
| `enforceCap` window hop | `91414f4` | newest-first trim parked a permanent interior hole that resume could never backfill | R3 review |
| alt frozen base AND height | `91414f4` | a smaller alt frame appeared to lower the retention bound below protected main rows | R4 review, unanimous |

All seven were spot-checked against the commits and the shipped comments; no
misattribution found. They are earned guards of a deliberately chosen model, not
accretion around a bad one, and removing them re-opens documented bugs.

## 5. What shipped

Status: the client half is implemented. The ED3 half in §2 was implemented,
reviewed, measured to lose data, and REVERTED; see §9.

The resume claim is `LineStore.replayBoundary()`, derived from one new scalar:

```ts
/** Lowest index at or above which content is not server-confirmed, or +Infinity. */
private unconfirmedFrom = Number.POSITIVE_INFINITY;

replayBoundary(): number {
  if (this.highest < 0) {
    return this.highest;
  }
  return Math.min(this.highest, this.unconfirmedFrom - 1);
}
```

A scalar rather than the window arithmetic the first draft proposed, because the
hydrate path needs the boundary PERSISTED and a persisted scalar subsumes the
derived expression. It also covers a case the window version could not: a screen
frame whose scroll chunks never arrived, which is ordinary truncation given that
the server writes each payload as its own message and ignores write errors.

Movement is asymmetric, and the asymmetry is the correctness argument:

- `markUnconfirmedFrom(base)` from a main-screen frame lowers it unconditionally.
  Every index in the window is a screen position the app repaints in place.
- `confirmRange(firstIndex, count)` from a scroll or history frame raises it, but
  only when the range REACHES the floor from at or below it. A chunk landing above
  a still-unconfirmed gap must not claim across it.
- `confirmRange` also HEALS: a range arriving entirely above `oldest` while the
  floor sits below `oldest` moves the floor up, because nothing it was protecting
  is held any longer. `applyScrollbackCleared` raises it to the erase bound for
  the same reason, directly.
- The alt branch of `applyScroll` confirms only the prefix its gate accepted.
- `applyHistoryScroll` deliberately confirms nothing: a paged reply sits strictly
  below the resident tail.
- `reset()` returns it to +Infinity, since a new epoch shares no indices.

Persisted as an OPTIONAL `unconfirmedFrom` on `StoreSnapshot`, omitted when
+Infinity, read back defensively. `SNAPSHOT_VERSION` stays at 1 under an explicit
carve-out added to its policy comment: the rule exists to prevent MISREADING, and
an optional field whose absence is defined as the pre-existing behaviour cannot be
misread. `fromSnapshot` reads fields individually off an `unknown` record, so an
older entry simply lacks the key, and an older reader never enumerates keys, so a
new entry is safe there too.

The consumer wires it defensively:
`getHaveThrough: () => replayBoundaryOf(render) ?? render.getHighestIndex()`. The
peer range spans engine majors and the accessor exists only from the minor that
added it; on an older engine the transport's own fallback would turn a missing
property into `haveThrough: -1`, a full replay on every attach, which is worse
than the defect being fixed. `replayBoundaryOf` is the single cast site and says
why. The exported `RenderHandle` deliberately does NOT grow: a required member is
source-breaking for any external structural implementor and no feature needs it.

## 6. The stranded band keeps its own definition

`predictReplayJump`'s band stays `keys <= sentHaveThrough`. The first review round
called it BLOCKING that the band no longer covers the old window rows and I changed
it to `max(sentHaveThrough, highest)`. Two existing tests refuted that, correctly:

- "predicts from the value this socket SENT" requires the band to be the 1000 rows
  at or below the sent value, NOT the 1200 later-delivered committed rows.
- "reclassifies the stranded band when the server's ring moved past haveThrough"
  requires 1000, not 1024 including the window rows.

The reasoning those tests encode: rows above the sent value arrived from the server
as committed content, so they are real history and must not become disposable
cache. The provisional rows do not need the band, because the replay now starts at
`boundary + 1` and covers them.

One residual, accepted and recorded rather than fixed: when the replay start is
CLAMPED above the boundary, the provisional rows are neither replayed nor banded,
so they stay tail-classified and miss the browse TTL sweep until cap eviction
reaches them. Widening the band to cover them would change a pinned invariant.

`predictReplayJump`'s `sentHaveThrough < oldest` early return is now reachable in
an ordinary shape, because a post-ED3 store has `oldest === win.base` and claims
`win.base - 1`. It remains the right answer, for a new reason: the band is empty
exactly when that fires, so the reclassify and the budget pass it gates are both
no-ops, and the descriptor retirement it skips exists only to protect a band that
does not exist. The comment states the new reason; a test pins it in both
directions.

## 7. Shapes rejected

**A separate live-window grid.** Ruled out by the founding decision, stated twice.
`docs/REBUILD.md` (deleted at `4a11184`, readable at `4a11184^`), 2026-06-29:

> Resolve the live/history split via one absolute-line-index buffer, not two data
> models. Why: a terminal is one numbered line stream; the dual model is the
> connective tissue that couples the bugs. Rules out: [...] a client-side
> live-zone-versus-history branch.

and `6c3758b`'s entry: "it does NOT reintroduce the client-side split." The
pre-rebuild client had two buffers and that is what the rebuild replaced, because
"every reconciliation point was a bug surface". `altRows` is the deliberate
exception, for a reason that does not generalise: alt content must not accrue
scrollback at all. A scalar is not that branch; it reads no second buffer.

**A `provisional: Set<number>`.** More mechanism than the harm needs. It would add
a second `X ⊆ lines.keys()` invariant beside `browse` plus a promotion path,
against a spec that records the cost of parallel bookkeeping (§5.3: a companion
counter "was wrong at four of them, it reached -96"). The scalar carries no
residency relationship at all, which is why `forget`, `enforceCap` and
`truncateBelowWindow` correctly leave it alone.

**Discarding the unconfirmed window band on detach.** Attractive because
`highestIndex()` would become correct by construction with no second accessor.
Rejected: it deletes rows the renderer is about to repaint from cache, which is
what makes a switch feel instant, and it moves `highest` on every switch so the
scrollback keeper writes an extra snapshot each time. It trades a wrong-content
artifact for a blank-band one.

## 8. Cost

Every attach replays up to one extra screen height. Derived from the encoder
(`wire_binary.go`: 19 bytes per SCROLL frame, 2 per row, 18 per run, plus UTF-8
text; 50-row chunks): about 0.1 KB for empty rows, 2.4-3.6 KB at one 40-byte run
per row, 4-6 KB at a full 80-byte run for 40-60 rows. Paid on every resume, not
only tab switches: backoff reconnects, heartbeat recovery, wake and online
reconnects. It is zero exactly when the benefit is zero.

Two accessors now exist with confusable names, and nothing enforces the choice.
That is the price of the additive route, mitigated only by the doc comments on both
and by the consumer test that pins which one the kernel prefers.

## 9. The ED3 half: implemented, then reverted

§2's second cause is real and reproduced: an over-height repaint after an ED3
pushes the erased frame off the top, those rows are appended to the ring the erase
had just emptied, and the clear signal ships ahead of them, so a client drops its
history and takes the dead frame back. A probe showed the composer row landing in
the ring at index 10.

A fix shipped as `ce0e90f` and was reverted as `eb07d60` after review, because it
was measurably worse than the defect:

- **Unbounded drop while detached.** `dropRedrawDrain` was released only inside
  `buildFrame`'s clear-signal block, which is unreachable with zero clients, and
  `redrawSettleUntil` never disarms on that path either. So "an ED3 inside the
  settle window" degenerated to "any ED3 after the last resize, while nobody is
  watching", and every drained line was discarded for the whole detached period.
  Measured: 200 lines of ordinary output printed while detached, ring length 0.
  Worse, one of the tests I wrote PINNED that as intended behaviour.
- **A retreating window base.** `flush_builder` computes
  `base = committedBefore + len(scrollOut)` before the drop and never recomputes
  it, so a dropped frame shipped a base ahead of `committed` and the next frame's
  base went backwards. The client had also just set `erasedThrough` and
  `pagingFloor` from the higher base, so the genuinely committed rows underneath
  were refused by apply-line guard 2b and unfetchable.

What a correct version needs: the drop bounded by a row budget captured at the ED3
rather than a latch with no lifetime on the detached path, `base` recomputed after
the drop so it never leads `committed`, and the settle hold evaluated in the
zero-client branch. That is its own change-set with its own review, not a rider.

## 10. Review record

Two rounds, three model families each, all against real artifacts.

Round one reviewed the design and returned AMEND from all three. It corrected the
public surface (no `RenderHandle` growth, defensive wiring), the naming (a replay
boundary, not a confirmation fact), the spec-amendment list, and the test plan; it
resolved the release question (the TS subpackage shares the root version, so a TS
major would force the Go module to `/v6`, which the additive route avoids); and it
raised the band concern that §6 records as refuted by the existing tests.

Round two reviewed this diff and returned AMEND, WRONG, AMEND. Three blocking
findings, all confirmed by execution before acting on them: the wedged floor (now
healed, §5), and the two ED3 defects above (now reverted, §9). The floor wedge was
the sharper lesson: measured at 99 while `highest` reached 297 and then 5223, it
would have turned every attach into a maximal replay and persisted across reloads,
which is the outcome the consumer wiring comment itself calls worse than the bug.
