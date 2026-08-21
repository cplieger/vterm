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

## 5. Decision

**Derive the replay boundary from state the store already holds, and decouple it
from the stranded-band input it currently shares.** Two changes, not one.

### 5.1 The wire value

```ts
/**
 * The resume replay-exclusion boundary: the highest index this client will not
 * ask the server to re-send. NOT a residency fact and NOT "the highest confirmed
 * row" — `win.base - 1` may be unheld or browse-classified. Only
 * `result <= highestIndex()` is guaranteed.
 */
replayBoundary(): number {
  if (this.highest < 0 || this.win.height <= 0) {
    return this.highest;
  }
  return Math.min(this.highest, this.win.base - 1);
}
```

The value is exact rather than conservative: the server pins `winBase := committed`
on every frame, "the base equals committed in all cases" (terminal.go:2513-2518),
so `win.base - 1` is the server's committed high-water mark as of the last frame
this client saw. That also bounds the extra replay to precisely the lines that
scrolled off while this client was away.

Sufficiency for the detached case: nothing moves `win` while a store has no socket.
`render.handleScreen` has one call site (ui `kernel.ts:649`), and neither `bind`
(render.ts:405-450) nor `captureViewMemory` (render.ts:1686) writes the descriptor.

### 5.2 The stranded band must keep crossing `win.base`

`sentHaveThrough` is not only a wire claim. It is fed back as `ack.sentHaveThrough`
and IS the definition of the stranded band (`predictReplayJump`, store.ts:1133-1160),
which `paged-scrollback.md:749-759` normatively requires to include the old window
rows and to cross `win.base`. Lowering the wire value alone breaks that, in two
reachable shapes:

- **Clamped replay.** When `replayStart` clamps to `committed - bound`
  (terminal.go:2409-2412), rows in `[win.base, replayStart)` are neither replayed nor
  reclassified. Today they become `browse`, which subjects them to the browse budget,
  the 60 s TTL sweep and the `freeze`/`pagehide` drop, so the artifact self-heals
  within a minute. Lowered, they stay TAIL and they are the NEWEST tail rows below
  the window, which `enforceCap`'s oldest-first walk reaches last. The artifact
  becomes strictly MORE persistent. Reachable at `tailCap` 1500 with a 50-row screen
  for any background tab that printed more than about 1450 lines, which is one build
  log.
- **The vacuity guard.** `sentHaveThrough < this.oldest` returns false early
  (store.ts:1144). Post-ED3 `applyScrollbackCleared` drops every line below base
  (store.ts:1579-1594), so `oldest === win.base` and `win.base - 1 < oldest` fires it.
  kiro-cli emits ED3 on every resize redraw, so this is the ordinary shape in the
  reporting app: nothing is reclassified and the descriptor is not retired.

So `predictReplayJump` takes the band top separately from the wire value. The
descriptor is still live when the ack is applied, so the window bottom at send time
is available locally:

```ts
const winBottom = this.win.height > 0 ? this.win.base + this.win.height - 1 : -1;
const bandTop = Math.max(sentHaveThrough, winBottom);
// vacuity guard and the stranded walk both read bandTop, not sentHaveThrough;
// `predicted` keeps reading sentHaveThrough, because that is what the server's
// reply is a function of.
```

About four lines. The prediction still derives from the sent value, which is §5.2's
actual requirement; only the band widens back to what it covers today.

## 6. Shapes rejected, with the reason

**A separate live-window grid (`WireRun[][]`, mirroring `altRows`).** Ruled out by
the founding decision, stated twice. `docs/REBUILD.md` (deleted at `4a11184`,
readable at `4a11184^`), 2026-06-29:

> Resolve the live/history split via one absolute-line-index buffer, not two data
> models. Why: a terminal is one numbered line stream; the dual model is the
> connective tissue that couples the bugs. Rules out: [...] a client-side
> live-zone-versus-history branch.

and `6c3758b`'s entry: "it does NOT reintroduce the client-side split." The
pre-rebuild client HAD two buffers and that is what the rebuild replaced, because
"every reconciliation point was a bug surface (duplicate rows, view-jumping, silent
resume misalignment)". `altRows` is the deliberate exception with a reason that does
not generalise: alt content must not accrue scrollback at all. A derived scalar is
NOT that branch: it reads the existing descriptor, like `inWindow`, `tailBound` and
`truncateBelowWindow` already do, and the unified thing was the data model.

**A `provisional: Set<number>` of unconfirmed rows.** Rejected as more mechanism than
the harm requires. It would add a second `X ⊆ lines.keys()` invariant beside `browse`
and a promotion path in `applyScroll`, against a spec that records the cost of that
bookkeeping (`paged-scrollback.md` §5.3: a companion counter "was wrong at four of
them, it reached -96 on a third successive jump ack"; the surviving design keeps "no
per-line flags").

**Discarding the unconfirmed window band on detach.** `forget` every key in
`[win.base, highest]` when the socket goes away, beside `bind`'s existing
`store.clearSolicited()` (render.ts:432). Attractive: `highestIndex()` becomes
correct by construction, no second accessor, no consumer change, and §9.3's
confusable-pair trap disappears. Rejected because it deletes rows the renderer is
about to repaint from cache, which is what makes a switch feel instant, and it moves
`highest` on every switch, so the scrollback keeper's advanced-only predicate writes
an extra snapshot each time and a window-only store transiently reports -1 into the
tabs catch-up cue (`tabs/index.ts:1753`). It trades a wrong-content artifact for a
blank-band artifact.

**A single scalar `unconfirmedFrom`,** lowered in `applyScreen`'s row loop and raised
when a durable frame lands. Deferred rather than rejected: it is the honest fix for
the attached-truncation residual in §9.2, about eight lines, and it subsumes the
derived value. Not needed for the reported harm, so it is not in this change-set.

## 7. Why an additive accessor rather than changing `highestIndex()`

`getHighestIndex` has two consumer meanings today and only one wants the boundary:

| Call site | Asking | Wants |
| --- | --- | --- |
| `render.ts:594` -> ui `kernel.ts:604` | the wire `haveThrough` | the boundary |
| ui `tabs/index.ts:1753,1793` (`< 0`) | "does this tab hold anything yet", gating the catch-up cue | highest held |
| ui `kernel.ts:1493,1744` (`>= 0`) | "did the hydrate restore anything" | highest held |
| ui `kernel.ts:1444` `session.highestIndex` | feature-facing content extent | highest held |
| ui `scrollback.ts:245,281` vs `snapshot.highest` | "has content advanced past disk" | must equal what `snapshot()` writes |

Changing the meaning would make a window-only store report `-1`, latching
`catchupWarranted` true forever, and would desynchronise the `savedThrough` watermark
from `snapshot.highest` so the background pass re-serialises the whole tail every
interval. It is also a breaking change to a published class. Verified during review:
the central release lane gives the TS subpackage the ROOT version
(`ci/.github/workflows/release.yaml:22-24`) and rejects a major whose Go module path
does not match (`:808-836`), so a TS `feat!` yielding 6.0.0 WOULD force the Go module
to `/v6` or fail the release. The additive route is engine v5.1.0 with `go.mod` left
at `/v5`.

## 8. Exact changes

Engine, `web/src`:

1. `store.ts`: add `replayBoundary()` next to `highestIndex()`, with the doc comment
   in §5.1 stating what it is NOT.
2. `store.ts`: `predictReplayJump` takes the band top from
   `max(sentHaveThrough, windowBottomAtSend)` per §5.2, and the vacuity guard reads
   the band top. `predicted` keeps reading `sentHaveThrough`.
3. `render.ts`: add `getReplayBoundary()` beside `getHighestIndex()` (render.ts:594).
   Leave `getHighestIndex` untouched.
4. `connection.ts`: no code change, correct the `getHaveThrough` doc comment
   (connection.ts:447-450), which describes the value as the highest held.
5. `README.md:80`: the `render` export list names `getHighestIndex` and needs the
   new export.

Consumer, `web-terminal-ui/src`:

1. `kernel/kernel.ts:604`: wire it DEFENSIVELY:
   `getHaveThrough: () => render.getReplayBoundary?.() ?? render.getHighestIndex()`.
   The peer range is `^3.10.0 || ^4.0.0 || ^5.0.0` (package.json:43) and admits
   engines with no such export, where `cb?.getHaveThrough?.() ?? -1`
   (connection.ts:1284) silently yields -1 and every attach requests a full
   2000-line replay. Three lines of fallback keep the range honest and avoid a UI
   major.
2. Do NOT add the method to `RenderHandle` (`kernel/types.ts:86`, re-exported at
   `src/index.ts:33`). It is an exported structural interface, so a required member
   is source-breaking for any external implementor, and no feature needs it. The
   optional call in item 6 needs no interface change.
3. Reconcile `jsr.json:25` (`^3.7.0`) with `package.json:43` while in there. Existing
   drift, not caused by this change.

Spec, `docs/paged-scrollback.md`:

1. §5.2's "`haveThrough` (the highest held index) is always in the live tail" is now
   false in both halves: the value is not the highest held, and `win.base - 1` may be
   unheld or browse-classified. Rewrite rather than tweak.
2. The stranded-band paragraph (:749-759) gains the §5.2 decoupling above. This is
    the load-bearing edit.
3. The replay formula's `max(0, ...)` rationale (:730-736) exists because an old
    window bottom "can legally sit AT or ABOVE" `committed`; unreachable now.
4. §4.5's parenthetical "(A test written with a `sentHaveThrough` BELOW the window
    base cannot see this)" now describes the shipping configuration and must be
    re-scoped.
5. §5.2's categorical "no window-derived bound may be EVALUATED while retired" gains
    the new bound, with its safe fallback stated (§9.5).
6. §4's mis-correlation proof HOLDS and slightly strengthens, because the boundary
    now names a committed immutable index instead of a mutating window row. Its scope
    note still needs the wording change.

## 9. Downsides against today

1. **Every attach replays up to one extra screen height.** Derived from the encoder
   (`wire_binary.go:504-535`: 19 bytes per SCROLL frame, 2 per row, 18 per run, plus
   UTF-8 text and URL bytes; 50-row chunks at terminal.go:2570-2577), the application
   cost for 40-60 rows is about 0.1 KB when empty, 2.4-3.6 KB at one 40-byte run per
   row, and 4-6 KB at a full 80-byte run. The earlier "5-10 KB" was a point estimate;
   10 KB needs dense styling or links. Measure a captured kiro frame before quoting a
   figure. The cost is paid on every resume, not only tab switches: backoff
   reconnects, heartbeat recovery, and wake/online reconnects
   (ui `kernel.ts:1297-1358`) all pay it, sometimes without any background output.
   The repo has cared about this before: `293b56d` exists because post-resize replay
   was "seconds of history visibly churning" on phones. It is zero exactly when the
   benefit is zero.
2. **The over-claim survives an interrupted dispatch.** §5.1's sufficiency argument
   covers the detached case only. While attached, `dispatchFrame` writes each payload
   as its own message under one 5 s context with errors ignored
   (terminal.go:1888-1892), screen before scroll, so "the screen frame at the new base
   arrived, the matching scroll chunk did not" is ordinary truncation: the socket
   dies, the context expires, or the tab switch's own `teardown()` lands between the
   two writes. Then `win.base` has advanced past rows whose committed copies never
   came and the boundary claims them. Residual, bounded by one dispatch's drained
   lines. The `unconfirmedFrom` scalar in §6 is the fix; it is not in this change-set.
3. **Two accessors, confusably named, and nothing enforces the choice.** The additive
   route buys its cheap release by leaving that trap in the published surface. A
   future consumer wiring `getHaveThrough` to the wrong one reintroduces this bug
   silently, and no test outside the engine catches it. Mitigated only by the doc
   comment in §5.1.
4. **The symptom survives in reduced form.** A page reload hydrates with no window
   descriptor, so the boundary degrades to `highest`. See §10.
5. **A new window-derived bound in the interval the descriptor is deliberately empty.**
   `paged-scrollback.md` §5.2 makes it a stated requirement that "no window-derived
   bound may be EVALUATED while retired", because `truncateBelowWindow`'s bottom would
   read -1 and delete the store. This bound is safe there by construction: `height <= 0`
   returns `highest`, which is today's behaviour, and the value is read at socket open
   before the ack that retires the descriptor. A second resume inside the retirement
   gap reads the fallback and over-claims exactly as today. A degradation to current
   behaviour in a rare window, not a new failure, and the requirement is categorical,
   so §8 item 13 amends it rather than relying on the fallback being obvious.
6. **`predictReplayJump` gains four lines and a second input.** The band no longer
   falls out of the wire value, so the two roles must stay deliberately separate. That
   is one more place a future edit can quietly re-couple, and §5.2's normative text is
   what guards it.

Against those: the current behaviour presents unconfirmed rows as settled history on
most switches, with no self-healing path short of cap eviction, and it is wrong
rather than expensive. A correctness defect the user sees daily outranks a few KB per
attach.

## 10. The hydrate path

`fromSnapshot` (store.ts:1384) sets `everEvictedThrough = oldest - 1` with no window
descriptor, so `replayBoundary()` degrades to `highest` and a reloaded page can still
show one stale band.

The first draft deferred this on the grounds that carrying a boundary in the snapshot
makes it a `SNAPSHOT_VERSION = 2` change under store.ts:37-42's meaning-change policy,
costing every stored entry one discard. Review disputed that, and the dispute looks
right: `fromSnapshot` validates field by field (store.ts:1384-1440), so an OPTIONAL
`replayBoundary` read as "absent means today's behaviour" is forward and backward
compatible without touching `v`. Verify that against the validator at implementation
time. If it holds, close this in the same change-set and delete §9.4; if the policy
is read strictly and a bump is required, defer it, because a fleet-wide snapshot
discard is not worth a page-reload artifact.

Either way the consumer coupling is unaffected: `scrollback.ts` compares
`store.highestIndex()` against `snapshot.highest` and both keep their meaning
(verified in review), so the three sites at :238, :245 and :281 need no change. If
the deferred variant lands later, that coupling is the thing to re-check first.

## 11. Test plan

Rewritten after review, which found three of the original cases vacuous. The rule:
every case below must fail against today's code for the intended reason.

RED tests, the regression evidence:

1. **Kernel wiring, real store.** A store holding history plus a live window, driven
   through production wiring, must send `haveThrough == win.base - 1`. Fails today at
   ui `kernel.ts:604`. The original plan's "the value sent is `replayBoundary()`" was
   vacuous: connection.ts:1284 already forwards whatever the callback returns, so a
   mocked callback passes today.
2. **End-to-end stale band.** Populate a store, capture the view, advance the server's
   base and content while detached, reattach through the real wiring, and assert the
   old window rows are REPLACED. Must derive the watermark through production code, not
   inject it, or it passes before the change.
3. **Stranded band still crosses `win.base`** (§5.2). After an ack whose replay start
   is clamped, every old window row is `browse`-classified. Fails today if the band is
   taken from the lowered wire value.
4. **The vacuity shape.** A post-ED3 store where `oldest === win.base`: the jump is
   still predicted and the descriptor still retired. Fails today if the guard reads
   the lowered value.
5. **Small-cap interior hole.** `effectiveTailCap` at or below `2 * win.height`, then a
   jump: assert no interior hole under the live window. This is the hazard the original
   plan missed while testing the wrong one (guard refusal normally coincides with the
   row already being absent, so asserting "`everEvictedThrough` does not refuse them"
   proves little).

Unit coverage, not regression evidence:

1. `replayBoundary()` equals `win.base - 1` with a window, `highest` without one, `-1`
   when empty, `-1` at `win.base === 0`, and the FROZEN main base minus one during alt.
2. A hydrated store reports it equal to `highest`, pinning §10 as intended behaviour.

Property suite:

1. `store.property.test.ts:339` drives the interleaved ack op with
   `haveThrough = s.highestIndex()`. Repoint it, or the suite keeps exercising the
   abandoned watermark and none of the interleavings that found four of §4's entries
   cover the new shape.
2. Add `replayBoundary() <= highestIndex()`. Note this passes a broken implementation
   that always returns `highestIndex()`, so it is an invariant, not a test of the fix.
   Equality does NOT imply `win.height === 0`: it also holds when `highest < 0` or
   `highest <= win.base - 1`.
3. `store.property.test.ts:209-210` (`highestIndex() === max retained key`) and
    :229-247 stay true and stay meaningful. Invariant 5's stated rationale, that the
    snapshot walk "keeps a resume's `haveThrough` from naming a refetchable line", no
    longer covers the sent value, which can now legally be browse-classified. Restate
    that rationale rather than deleting the assertion.

## 12. Review record

Three adversarial seats, all AMEND, no seat calling the decision wrong.

Sustained by two or more seats, folded in above: the public `RenderHandle` must not
grow and the consumer must wire defensively (§8 items 6-7); the value is a replay
boundary and not a confirmation fact, so both the name and the property claim were
wrong (§5.1, §11 item 9); §8's spec-amendment list was incomplete (items 9-14); the
test plan was partly vacuous (§11).

Single-seat findings accepted after verification against the code: the
`sentHaveThrough` double duty, which was the one blocking finding and is now §5.2;
the ED3 second cause, now §2; the interrupted-dispatch residual, now §9.2; the
snapshot-version dispute, now §10; and the release lane's shared-version rule, which
resolved the earlier UNVERIFIED note in §7.

Corrected in passing: §9.6 previously claimed the browse band WIDENS, which had the
sign backwards; the harm statement claimed the artifact "survives an app restart"
while §10 defers exactly that path; `replayMaxForResume` needs no change but for the
reason that re-delivered rows land on indices already held, not because its window
reservation matches the extra replay; and five line references were stale.
