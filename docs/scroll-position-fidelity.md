# Design: scroll-position fidelity

Status: IMPLEMENTED — r1-reviewed (two reviewers), revised three times
(2026-08). Targets the SAME engine minor as demand-paged scrollback
(`docs/paged-scrollback.md`), whose server and client sides are both
implementing now — §7 is a live coordination section, not a
future-compatibility note, and §7.3 records a confirmed defect in that
in-flight code that this design does NOT fix (§7.3 routes it).

Deliberately touches NO file the paging work owns: the r2 draft's
`StoreChanges` change is gone (§5 is renderer-local now), so the surface is
`web-terminal-engine/web/src/{scroll,render}.ts` plus
`web-terminal-ui/src/features/tabs/**`. Rationale in §9.1.

## 1. Problem

Two reported symptoms, one audit (2026-08), four defects. The scroll
controller itself is not among them: `scroll.ts`'s follow/hold state
machine and its direction asymmetry are correct and well covered. Every
defect is at a SEAM around it — the tab-switch restore, the read anchor's
recovery path, and the test fixtures that made both invisible.

### 1.1 Symptom A — returning to a tab does not restore the bottom

`switchTo` saves a PIXEL offset when the user leaves a tab
(`web-terminal-ui/src/features/tabs/index.ts:1507-1508`) and replays it
one animation frame later (`:1577-1580`):

```ts
const view = { top: next.scrollTop, following: next.following };
requestAnimationFrame(() => { ctx.scroll.restoreView(view); });
```

`ctx.render.bind(next.store)` (`:1545`) wipes the DOM synchronously
(`render.ts` `rebuild()` → `output.replaceChildren()`) and re-queues every
retained line; the renderer builds at most `MAX_ROWS_PER_FRAME = 300` rows
per frame, plus the force-built cursor row. `bind`'s flush registers its
rAF FIRST (both are registered in one synchronous task, and rAF callbacks
run in registration order), so at restore time the scroller holds ~301
rows — about 5117 CSS px at the 17 px row height in
`web-terminal-ui/css/02-terminal.css`. A full tab is `MAX_LINES = 5000`
lines ≈ 85,000 px, so a saved offset from the bottom is ~84,200 px.

The browser clamps that write to `scrollHeight − clientHeight`.
`writePreservingFollow` reads the clamped value back and accepts it
(`scroll.ts`), and the rAF is fire-and-forget — there is NO retry after
the drain finishes. Consequences:

- For any tab holding more than ~300 lines the POSITION half of the
  restore never lands. The outcome is decided entirely by the `following`
  boolean: left following, the per-frame `stickToBottom` walks the
  viewport back to the bottom over the next ~17 frames (right answer,
  reached by accident); left holding, nothing re-pins and the viewport
  stays wherever the clamp left it.
- A pixel offset is the wrong thing to save even when it does land. The
  tab's content GROWS while backgrounded: `connection.setSession` calls
  `reconnectNow()` and the server replays everything the client missed, so
  the same offset denotes a different line than when the user left.

Under paging's 1500-line resident tail the window narrows to ~5 frames and
~25,500 px. It does not close: the clamp still discards the offset.

### 1.1.1 What this does NOT yet explain

Honest limit, and the reason §1.3 exists. A tab left genuinely FOLLOWING
is recovered by the per-frame pin, so the reported "I was at the bottom
and came back higher" requires the tab to have been HOLDING while visually
at the bottom. §1.3 gives two mechanisms for that and neither is proven
against a live session — they are code-grounded hypotheses. Any
implementation MUST land the §8 instrumentation assertion (log the saved
`{abs, following}` pair on every switch) so the mechanism is confirmed
rather than assumed. If the pair reads `following: true` on the failing
switch, §1.3 is wrong and the diagnosis needs to reopen.

### 1.2 Symptom B — random jumps that coincide with SIGWINCH

When the anchored row element is gone, `restoreReadAnchor` re-resolves by
ABSOLUTE INDEX — `firstRowAtOrAfter(anchor.abs)` — and pins whatever it
finds to the reader's old screen position. The only stand-down is
`fullResetThisPass`, which covers a server-restart epoch change only.

On every resize redraw kiro-cli emits ED3. The server clears its
scrollback ring but PRESERVES the monotonic commit counter
(`terminal/scrollback.go`: "committed is preserved so absolute indices
never repeat within a session"), so the reprinted transcript commits at
new, higher indices while the client deletes the old copies via
`applyScrollbackCleared(msg.base)`. The anchored index is therefore not merely
missing — the "nearest surviving row at or after it" is content from a
DIFFERENT region, and the correction drags that row to where the reader
was looking. Fires only while scrolled up, which is why it reads as
intermittent.

The same function's other recovery path compounds it:
`captureReadAnchor`'s fallback anchors the LAST row when everything is
above the viewport top ("a shrink already clamped past the last row"), so
after a large shrink the correction holds the tail rather than a reading
position.

Scope honesty (r1 finding): §5 fixes the client's misplaced correction. It
does not make the reader's line survive a SIGWINCH — that line is
destroyed server-side (§6.1) and re-emitted at a new index. §5's win is
"stop teleporting the reader to unrelated content", not "hold the line
across a resize".

### 1.3 The trap that makes 1.1 bite

"Holding at the bottom" is a real, documented state and it is STICKY: only
a keystroke or the jump button clears it, and `restoreView` deliberately
carries it across a tab switch. Visually it is indistinguishable from
following.

Entering it takes one upward classification leaving more than
`CLAMP_EPSILON_PX = 1` below. Two code-grounded routes:

1. `scrollHeight` and `clientHeight` are INTEGER-ROUNDED while `scrollTop`
   is fractional, so under browser zoom or a fractional device pixel ratio
   the residual can exceed 1 px.
2. `viewport.ts` writes `style.top`/`style.bottom` synchronously per
   `visualViewport` event. A touch nudge during an iOS keyboard slide, with
   `clientHeight` mid-change, is an upward move with a real gap.

Either way the epsilon is being asked to IDENTIFY a browser clamp from its
arithmetic signature, when the code that caused the clamp knew a shrink
was coming. §4 replaces the inference; §1.1.1 governs whether this is the
symptom's actual cause.

### 1.4 Why none of this was caught

The fixtures that would catch §1.1 are built so they cannot. Both scroll
fixtures define `scrollTop` as a plain setter with NO clamping
(`scroll.test.ts` `makeScrollEl`, `render-read-anchor.test.ts`); exactly
one test in the suite clamps like a real container. `tabs/index.test.ts`
mocks `restoreView` as a bare `vi.fn()`, so the switch's save/restore
semantics are never asserted end to end. No test anywhere asserts
`scrollTop` across a multi-frame drain.

This matters beyond the present bugs: `paged-scrollback.md` §7 already
specifies "anchor preservation across a multi-frame 1000-row prepend". On
a non-clamping mock that test cannot fail for the reason it exists.

## 2. Principles

1. A reading position is a LINE, not a pixel offset. Pixels are derived.
2. Whoever CAUSES a content-height change announces it — and only when it
   will actually move the viewport. An announcement that may not be
   consumed is the same bug class as the epsilon it replaces.
3. A correction that cannot identify the reader's line MUST stand down,
   never guess. Leaving the viewport where it is beats teleporting it to
   unrelated content.
4. `scroll.ts` stays index-free and DOM-row-blind. Absolute indices and
   row geometry live in `render.ts`, which already owns that mapping. (The
   same split `paged-scrollback.md` §5.4 makes for the fetch controller.)
5. Nothing here may fight the user. Any real gesture cancels any pending
   library restore — and "real" MUST be established by the library
   knowing what it itself wrote, not by a signal that also fires for the
   browser's clamps.
6. One row-selection PRIMITIVE, not one policy answer. Callers asking
   different questions about "the viewport" get different answers from the
   same measurement.

## 3. Fix 1 — anchor-based per-tab view memory

### 3.1 The saved value reuses the shipped anchor shape

r1 finding (both reviewers, converged): the r1 draft's bespoke `ViewAnchor`
with a `rowOffsetPx` residual was both redundant and wrong. Redundant
because `render.ts` already has the shape; wrong because the row-selection
rule (`offsetTop >= scrollTop`, the first row at or BELOW the viewport
top) makes the viewport sit ABOVE the selected row, so a residual declared
as `0..rowH` has the opposite sign, and because adding
`row.offsetTop + residual` mixes `.term-output` space (`position: relative`,
so rows report offsets in output space) with `.term`'s `scrollTop` —
the exact trap `render.ts`'s `rowTopInTermWrap` exists for, whose comment
records that it "floated 4px above every glyph under the real stylesheet —
invisible to the harness".

So the saved view is the existing `ReadAnchor`, promoted from private to a
named export, plus the follow flag:

```ts
interface ViewMemory {
  abs: number;        // absolute line index of the anchor row
  screenTop: number;  // el.offsetTop - scrollTop, i.e. a DIFFERENCE
  following: boolean;
}
```

`screenTop` being a difference is what makes it space-agnostic: the
restore's correction is `el.offsetTop - currentScrollTop() - screenTop`,
identical arithmetic to `restoreReadAnchor`'s drift, so both terms are in
the same space whichever space that is. No coordinate conversion, no
`rowTopInTermWrap` call, no new sign convention.

### 3.2 Renderer seam

`render.ts` gains:

- `captureViewMemory(): ViewMemory | null` — `captureReadAnchor`'s
  selection WITHOUT its `!isUserScrolledUp()` early-return (the follow
  half is saved alongside, and paging needs the measurement while
  following), returning `abs`/`screenTop` plus the current follow state.
- `bind(next, opts?: { view?: ViewMemory })` — see §3.3. There is no
  separate `restoreViewAnchor` export and no public `setFollowing`.

A MARKER IS NEVER AN ANCHOR. Selection considers only children tracked in
`rowEls` — content rows. This closes the audit's trim-marker-as-anchor
case and is a prerequisite for `paged-scrollback.md` §5.4's per-gap
markers, which carry a real `data-abs` and would otherwise be selectable
as reading positions.

### 3.3 The view swap is ONE atomic renderer operation

r1 finding (gpt): exporting `setFollowing` hands every consumer a raw
override of the state machine `scroll.ts`'s 74-line header spends its
length defending, and `web/src/index.ts` re-exports the whole scroll
namespace, so "internal" is not available. Instead the swap is one call:

```ts
ctx.render.bind(next.store, { view: next.view });
```

`bind` internally, in order: adopt the follow half synchronously (through
the existing `restoreView({ top: currentScrollTop(), following })`, which
sets the flag without moving the position), wipe, seed the drain (§3.5),
arm the restore under a fresh generation (§3.4), schedule the flush.

Adopting the follow half BEFORE the wipe is what makes frame 1 correct
instead of corrected: today's deferred `restoreView` leaves frame 1's
`stickToBottom` gated on the OUTGOING tab's flag — the bug the comment at
`index.ts:1565-1576` records and repairs one frame late.

### 3.4 The position half is a generation-tagged transition

For a tab left FOLLOWING no position is restored: the pin is the correct
answer and is already applied every flush. `bind` MUST NOT arm in that
case, which removes the clamp problem from the common path entirely.

For a tab left HOLDING, `render.ts` keeps:

```ts
let pendingRestore: { view: ViewMemory; gen: number; lastWrote: number } | null
let bindGen = 0   // ++ on every bind(); stamped into pendingRestore.gen
```

The restore is consumed in `flushRender`'s tail:

```text
flushRenderInner()            // mutations
applyPendingRestore()         // owns the position while armed
restoreReadAnchor(anchor)     // NOT skipped — see below
stickToBottomIfFollowing()
[paging: fetch trigger]       // §7.4 — MUST be last
```

`applyPendingRestore` looks up `rowEls.get(view.abs)`. If present, it
writes the drift correction (§3.1) through the follow-preserving write,
records the resulting `scrollTop` in `lastWrote`, and DISARMS. If absent,
it leaves the position alone and stays armed.

`restoreReadAnchor` is skipped ONLY in the frame the restore actually
LANDED (implementation finding, §11 r4 — the r3 draft said "not skipped"
and its reasoning was wrong). The anchor is captured BEFORE the frame's
mutations, so once the restore has authoritatively moved the viewport the
anchor's drift measures the restore's own write and corrects it straight
back out: the two cancel exactly and the reader stays where the clamp left
them. Skipping it while merely ARMED would be wrong for the opposite
reason — a rebuild spans many frames, and suppressing the anchor across
all of them reintroduces the WebKit read-position slide for every one.
`applyPendingRestore` returns whether it landed, which is exactly the
discriminator.

TERMINATION — first of:

- the anchored row was built and positioned (success);
- a CANCEL event (below);
- the STORE-LEVEL settle: `renderQueue.size === 0` AND no inbound frame
  for the bound store in the last 250 ms. `renderQueue.size === 0` alone
  is NOT a terminal signal (r1 finding, gpt) — `render.ts` documents that
  it "reaches zero between the server's replay chunks", the same trap the
  tabs feature's `catchupWarranted` comment records for the catch-up cue.
  The ALT case satisfies this immediately and correctly: `rebuild()` skips
  `queueRowsViewportFirst` when `store.isAlt()`, so the queue is empty on
  frame 1 and there is no scrollback to restore into;
- 2000 ms absolute bound.

A stale generation is itself terminal: any `bind` invalidates a prior
pending restore, so a second switch mid-drain cannot land the first tab's
anchor into the second tab's store. Ordering is cancel-then-arm, one slot.

CANCEL is exactly two things, established by SELF-KNOWLEDGE rather than by
a notification (r1 finding, claude H1, confirmed): `onScrollPosition`
fires at `scroll.ts:159` BEFORE the direction branch, so it fires for the
browser's own clamps — including the clamp the rebuild itself causes.
Wiring cancel to it makes the rebuild cancel the restore that rebuild
exists to serve. Instead:

1. `applyPendingRestore` compares the observed `scrollTop` against
   `lastWrote` (seeded at arm time with the post-wipe value): an
   unexplained divergence is a gesture and cancels. An accepted input byte
   is covered by this for free, since `sendBytes` moves the position via
   `scrollToBottom`.
2. `ch.fullReset` — indices restarted, so the anchor is meaningless.

There is deliberately NO cancel-on-region-discard (r1 finding, claude M4).
Under `paged-scrollback.md` §5.6 a backgrounded tab's cache is dropped by
its own TTL while its store is unbound, so a discard flag accumulates and
drains on the first flush after `bind` — inside the restore window. That
rule would lose the restore on every re-entry to a TTL'd tab, including
the common case where the anchored row is in the resident tail and would
have restored fine. The row lookup already distinguishes the two cases for
free.

On any non-success termination the viewport stays where it is with the
saved follow state applied — the honest degradation: the reader's line is
genuinely unavailable, and the tail with follow OFF is what that means.

TEARDOWN: the pending restore and its timer are cleared by `render.init`,
`bind`, `scroll.init`, and the ui's `destroy()`. An uncancelled timer
writes `scrollTop` on a detached element after teardown. `scroll.init`
already resets `preserveFollowOnce` for the same reason; the new state gets
the same treatment.

### 3.5 Deferred: seeding the drain around the restore target

The r2 draft had `bind` seed the drain with `[abs, abs + windowHeight)`
right after the live window, so a deep reader's anchor row is built in
frame 1 or 2 instead of frame ~11 of a 17-frame drain.

DEFERRED (r1 finding, claude counter-recommendation 3). It buys ~180 ms of
visual latency in the deep-reader case only; `armCatchup` already covers
that wait; the restore is idempotent so the outcome is identical either
way; and under paging's 1500-line tail the whole drain is ~5 frames. Against
that it changes `rebuild()`'s signature and `queueRowsViewportFirst`'s
ordering — both of which the in-flight paging branch depends on (§9.1).
Land §3.1–§3.4, measure, then decide.

### 3.6 Interactions

Each of these is a real code path the r2 draft did not mention (r1 finding,
claude counter-recommendation 6).

- **Alt screen.** `captureViewMemory` returns null while `store.isAlt()`:
  an alt grid has no absolute indices worth remembering, and measuring one
  would overwrite a tab's real saved anchor with an alt-screen row. A tab
  left in alt therefore restores as "following", which is what alt means.
- **Predictive echo and the IME composition view.** Both position from a
  fallback (`padT + row * cellHeight`) when the target row is unbuilt, and
  that fallback is window-relative — wrong whenever the viewport is not at
  the window, which is precisely the armed window. The switch path is
  already safe because `performSwitch` calls
  `composition.cancelComposition()`, but a resume replay inside the 2000 ms
  window is not. Neither is made worse by this design; both are recorded
  as pre-existing and out of scope.
- **Text selection.** `rebuild()`'s `replaceChildren()` destroys it, today
  and after this change. The repo treats selection survival as first-class
  elsewhere, so this is worth stating: a view swap is not selection-
  preserving, and this design does not change that.
- **The `wt-switching` animation.** It toggles a surface class in the same
  frame band as the restore. Per the CSS scroll-anchoring spec a `transform`
  on the path to the scroller SUPPRESSES native anchoring, so during the
  animation the manual correction may be the only one running — which is
  the case §3.1's arithmetic already handles, since it measures residual
  drift rather than assuming either behavior.
- **Persistence is out of scope.** `ViewMemory` lives in memory only. A
  page reload resets every tab to following, exactly as today. Persisting
  it alongside `scrollbackKeeper`'s snapshot would require validating the
  anchor against the epoch seed, and a restored-but-unverified anchor is a
  worse failure than starting at the tail.

## 4. Fix 2 — position-gated shrink reconciliation

r1 finding (both reviewers, converged): the r1 draft's bare
`noteContentShrink()` reproduced exactly the bug `preserveFollowOnce`
avoids. `preserveFollowOnce` arms only when the write actually MOVED the
position, precisely so it cannot linger and swallow a later gesture; an
announcement made before every row removal carries no such evidence. Rows
removed BELOW the viewport, removals smaller than the remaining bottom
gap, and a rebuild of an already-short surface all produce no clamp and no
scroll event — and claude found a concrete instance: `rebuild()`'s wipe
often does not shrink `scrollHeight` at all, because the caret overlay is
an absolutely-positioned `.term` child left at the old cursor row's `top`
and `rebuild()` never moves it. The arm would then be set on every tab
switch and swallow the incoming tab's next real gesture.

The API is therefore position-gated and transactional. The signature reflects
what IMPLEMENTATION found (see §11 r4): the caller passes the offset it read
BEFORE its mutation, and the arm is decided on OBSERVED movement rather than a
predicted height, which is strictly stronger — a predicted height can be wrong
in both directions, an observed move cannot.

```ts
/** Announce that the caller's own mutation just REMOVED content, so the scroll
 *  event it produced is the browser's clamp and not a gesture. Call AFTER the
 *  mutation, passing the offset read BEFORE it. Arms only when the position
 *  actually moved — a move guarantees an event to consume the arm, so it cannot
 *  linger — and is cleared unconditionally by the first event either way. */
export function noteContentShrink(scrollTopBefore: number): void;
```

The renderer calls it in `flushRender` immediately after `flushRenderInner`
and BEFORE any write of its own, or the comparison measures that write instead
of the browser's clamp. The scroll handler's upward branch then reads:

```text
upward move:
  if a shrink was armed  -> consume, preserve follow      (a clamp)
  else if distanceFromBottom() > CLAMP_EPSILON_PX -> follow = false
  else preserve                                          (subpixel residual)
```

Two properties make this safe where the r1 draft was not: the arm cannot
be set when no clamp is possible, and it cannot outlive its own flush.
`CLAMP_EPSILON_PX` survives with its ORIGINAL job only — absorbing
fractional-layout rounding — and stops being the thing that IDENTIFIES a
clamp.

This is the fix that generalizes. Under `paged-scrollback.md` content
shrinks stop being rare: browse-cache eviction at every page apply, the
cap-flip reclassify drain (up to 3000 lines in three removals), and the
§5.6 TTL `dropBrowseCache`.

Scope note: this does NOT change what a shrink does to the POSITION. The
anchor owns that. It changes only whether a shrink can silently switch
auto-follow off.

### 4.1 What is explicitly not proposed

Re-engaging follow because the viewport is observed at the bottom. That is
the position-derived rule `scroll.ts`'s header documents as the cause of
the measured ED3 pin-to-tail bug. Follow keeps engaging only on init, an
explicit call, or a downward gesture reaching the bottom.

Showing the jump-to-bottom button while the viewport is visually at the
bottom stays as-is: with follow off, an affordance that resumes follow is
correct, and it is the only cue the state has.

## 5. Fix 3 — the anchor stands down on a region discard, renderer-side

The r2 draft added `StoreChanges.discardedRanges`. That is WITHDRAWN, for
two reasons that happen to agree: `store.ts` is the file the paging work is
actively rewriting (§9.1), and the renderer already has everything it needs
without a store change.

`render.ts` is the CALLER of every region-discard path. `handleScreen(msg)`
sees `msg.scrollbackCleared` and `msg.base` before handing the frame to the
store; `applyHistoryScroll` and `dropBrowseCache` are invoked from
`render.ts:467` and `:503`. So the renderer can record a discard at its own
call sites, with the exact bound, and no new store surface:

```ts
// Set in handleScreen when msg.scrollbackCleared, cleared at the end of the
// flush that observes it. The base BELOW which history was discarded.
let discardedBelowThisPass = -1;
```

`restoreReadAnchor`'s recovery path becomes:

```text
anchor element gone:
  fullResetThisPass                   -> stand down  (indices restarted)
  survivor.abs >= discardedBelowThisPass > -1
                                      -> stand down  (NEW: the span between
                                         the anchor and the survivor was
                                         discarded, not cap-trimmed)
  otherwise                           -> firstRowAtOrAfter(abs)
```

This is exact rather than a proxy. A cap trim always removes the OLDEST
contiguous run, so its survivor is the new oldest and is the reader's
neighbouring CONTENT — today's re-resolve is right and stays. An ED3
discards everything below `msg.base`, so a survivor at or above that base
is separated from the anchor by the discarded span, and re-anchoring on it
would pull unrelated content to the reading position. That is §1.2's bug,
and this is the minimum test that distinguishes the two.

Browse eviction under paging needs NOTHING here, and the reason is in that
design: its far-edge removals EXEMPT every line within `prefetchThreshold`
of the viewport ("an eviction must never create a hole the trigger
immediately re-fetches, nor blank the rows under the reader"), so browse
pressure cannot remove the anchored row. If that exemption is ever
weakened, this rule needs the paging call sites added — recorded so the
dependency is explicit rather than re-derived.

`truncateBelowWindow` contributes nothing either (r1 finding, claude M6 —
the r2 draft left it undecided): it removes rows BELOW the window bottom,
which are below every possible reading position, so no anchor can be
stranded and no adjacency can break.

`captureReadAnchor`'s last-row fallback is narrowed at the same time: it
applies only when the container is SHORTER than the viewport (nothing else
to anchor). When rows exist below the viewport top the search cannot
legitimately fail, and using the tail as a proxy reading position is what
turns a large shrink into a tail-drag.

An index-generation counter was considered and rejected: `committed` is
monotonic, so indices are never REUSED within a session; the defect is
adjacency, not identity.

Non-goal, recorded so it is not re-derived: re-finding the reader's line by
matching reprinted TEXT. kiro-cli's redraw re-emits the same characters at
new indices, so content matching is theoretically possible and is a
different (much larger) design.

## 6. Smaller items in scope

- `updateFontMetrics` MUST capture-then-restore, not merely schedule a
  flush (r1 finding, gpt): it mutates `cellHeight`, letter-spacing, and
  the CSS variable BEFORE any scheduled flush, so a flush-time capture
  reads post-change layout and the reader's line is already gone. The
  sequence is: capture a `ViewMemory`, apply metrics, then restore that
  captured anchor in the scheduled flush. A test that only asserts "a
  flush was scheduled" passes on the broken version.
- `restoreScrollTop` is DEPRECATED for one release, not deleted (r1
  finding, both reviewers): `web/src/index.ts` re-exports the whole scroll
  namespace and `web-terminal-ui/src/kernel/types.ts` publishes the method,
  so an in-repo caller search does not disprove external callers. Removal
  rides the next major.

### 6.1 Out of scope, with reasoning

- **`viewport.ts`'s un-debounced `style.top`/`style.bottom` writes.** They
  must stay synchronous — that is what keeps the terminal over the visible
  area during an iOS keyboard slide. §4 handles the follow consequence of
  the clamps they cause; the position consequence is the anchor's.
- **Server-side height shrink destroying bottom rows**
  (`vt/screen.go` `resizeHeight` truncates `s.Cells[:rows]` without
  appending to `Drained`, so a later grow restores blanks under any child
  that does not repaint on SIGWINCH). Committing those rows to scrollback
  instead is a change in TERMINAL SEMANTICS, not scroll fidelity: the ring
  holds lines that scrolled off the TOP, and the mode-aware grow hack
  (`InAltScreen` prepend vs normal append) exists because the current
  bottom-drop already forced a compensating choice. It needs its own
  design, behavior-manifest entries, and review. This is also the reason
  §1.2's fix is partial, stated there.

## 7. Relationship to `docs/paged-scrollback.md`

That design is RATIFIED and rewrites residency: a 1500-line resident tail
plus on-demand paging of older history, with per-gap markers, a fetch
controller in `render.ts`, and a browse cache under a 5-minute TTL.

IMPLEMENTATION STATE, re-verified after r1 (the r1 draft's account was two
commits stale): the server side is implementing
(`terminal/history_paging_test.go`, `wire-golden/resumeack-historypaging.bin`).
Client side, LANDED AND WIRED: `intervals.ts` (now purely the
gap-geometry source); `store.ts`'s `effectiveTailCap`, the `browse` key
set, `solicitedPending`, `pagingFloor`, `applyResumeAck`, `confirmPaging`,
`predictReplayJump`, `isFollowing`, `dropBrowseCache`; `scroll.ts`'s
`onScrollPosition` seam (after the `preserveFollowOnce` early-return,
exactly as its §5.4 specifies); `render.ts`'s `viewportAbs()` and its call
sites. `connection.ts` invokes `onResumeTransition`, and `kernel.ts` wires
the full seam set plus the browse-cache TTL sweep, so the ack transition is
reached from the wire.

So §7.1 and §7.2 are CONSUMED, not introduced. The seams exist; this
design's job is to use them correctly. §7.3's defect is fixed.

### 7.1 Consumed seam: `onScrollPosition`

It is in the tree and its placement is as specified. This design does NOT
use it for cancellation — see §3.4, and claude's H1: because it fires
before the direction branch it also fires for the browser's clamps,
including the rebuild's own.

That has a consequence for PAGING, not just for this design: the fetch
trigger hangs off this seam, so every content-shrink clamp — cap eviction,
ED3, a tab-switch wipe — fires a trigger evaluation. The trigger's own
guards make each one harmless (pacing bucket, single-flight, threshold),
so this is noise rather than a defect, but it is worth a comment at the
seam so the next reader does not conclude the seam means "the user
scrolled".

### 7.2 Consumed primitive: one measurement, not one answer

`viewportAbs()` exists at `render.ts:1180` and has three callers with
DIFFERENT questions (r1 finding, both reviewers): `applyResumeAck` asks
which reclassified rows must survive the switch; `applyHistoryScroll` asks
which rows are live now for far-edge eviction exemption;
`dropBrowseCache(pageVisible=true)` asks whether a visible reader is
sitting on cache; the fetch trigger asks which gap the interaction is
approaching.

The r1 draft mandated one function returning one answer. That is withdrawn
(principle 6). The contract is:

- ONE row-selection primitive — `captureViewMemory` — so there is a single
  definition of "the row at the viewport top".
- `liveViewportAbs()`: the measurement as it stands. Keeps today's callers
  unchanged.
- `pendingRestoreAbs()`: the armed restore's `abs`, or null.
- Only `applyResumeAck` consults the pending value, and only after
  validating same store and same bind generation. The other three callers
  keep the live value — for them the pending anchor is the wrong answer,
  because they are asking about NOW (what to exempt from eviction, what
  the reader is looking at, which gap is being approached).

### 7.3 A confirmed defect in the landed ack transition (FIXED)

The r1 draft claimed the containment rule reads a stale global follow
flag. That is REFUTED (both reviewers): `store.ts`'s `isFollowing`
(`:923-928`) derives following from `viewportAbs >= win.base`, never from
`scroll.ts`'s flag. The r1 mechanism was wrong.

The substance survives in a sharper form, and claude's H3 found the actual
bug, which this audit confirms by reading the code:

`predictReplayJump` (`store.ts:982-1001`) does
`this.win = emptyWindow()` — retiring the window descriptor, deliberately,
so the jump band may include the old window rows — and it does so BEFORE
`applyResumeAck`'s step-5 containment test. The code AS IT WAS when this
was found (the interval set has since become a key set, and the test now
reads a follow answer captured before step 4):

```ts
const inside = this.isFollowing(ack.viewportAbs)
  ? false
  : intervalsContain(this.browseIntervals, ack.viewportAbs);
```

`emptyWindow()` has `height: 0`, and `isFollowing` requires
`this.win.height > 0`. So on EVERY predicted replay jump `isFollowing`
returns false, the "a FOLLOWING viewport is outside every reclassified
band by definition" shortcut is unreachable, and the test falls through to
`intervalsContain`. But `predictReplayJump` has just moved
`[oldest, sentHaveThrough + 1)` — which includes the old window rows the
following reader was looking at — into `browseIntervals`. So
`intervalsContain` returns TRUE, `inside` is true, and the pass drains to
`BROWSE_CACHE_CAP` (2500) instead of `PREFETCH_THRESHOLD` (500).

`paged-scrollback.md` §5.3 specifies the opposite in normative terms: "a
FOLLOWING viewport is OUTSIDE every reclassified band by definition (it is
looking at the live tail, not at cache — the common case, and the one §3's
memory picture is keyed on)". The replay-jump path is the one that doc
names as the primary real population. So the store retains ~2000 lines
more than the design's memory budget on exactly the case it was keyed on.

FIXED on the paging branch, the first of the two ways named here:
`applyResumeAck` captures the follow determination into
`followingBeforeJump` BEFORE step 4 can retire the window, and the
containment test reads that instead of asking afterwards. Pinned by
`store-resume-containment.test.ts`.

That fix has since been superseded by one that removes the hazard instead
of sampling around it: the store no longer decides whether the reader is
following at all. `applyResumeAck` takes `following` as an INPUT and this
layer supplies it, from its own scrolled-up state with an armed restore
overriding — a restore is armed only for a position in history. Retiring
the window descriptor mid-transition can no longer affect the answer.

This design's `pendingRestoreAbs()` is why the intermediate step was not
enough. The membership half of the old test asked whether the reader's row
was a cache row, and a remembered anchor's row may since have been
dropped, which is precisely why the live `viewportAbs()` is not used
there. So a restore into history read as "not on cache" and drained to the
small target: measured 801 rows retained rather than 2420. The reader kept
their position but lost their depth. Passing the fact removes the question,
and §7.2's anchor is now honored in both halves of the transition rather
than one.

ROUTING (r1 finding, claude counter-recommendation 1): recorded in this
section only because this is where it was found. The fix lives in
`applyResumeAck`; `paged-scrollback.md` §5.3 carries the normative rule.

The transient-viewport concern that motivated the r2 draft's §7.3 also
survives, separately: a tab switch does reconnect
(`tabs/index.ts:1545-1546` → `kernel.ts:918` → `connection.setSession` →
`reconnectNow`), so once `onResumeTransition` is wired the transition will
run with a viewport measured mid-rebuild. §7.2's `pendingRestoreAbs()` is
the answer when this design's branch is present; absent it, the paging
work should treat a viewport measured during a fresh bind's drain as
UNKNOWN and take the conservative branch.

`paged-scrollback.md` §7 should gain two integration rows: "a following
reader across a replay jump drains to 500, not 2500" and "a tab switch
whose resumeAck carries a cap flip does not evict the incoming tab's
reading position".

### 7.4 Ordering inside `flushRender`

Paging's fetch trigger MUST be last in the tail (§3.4's listing). Fired
before the restore it would compute its prefetch window from the
pre-restore viewport and fetch the wrong page — a wasted request against a
paced bucket, and a marker reading "loading" for a gap nobody is
approaching.

### 7.5 Where the two designs help each other

- Paging's 1500-line tail narrows §1.1's window from ~17 frames to ~5; it
  does not close it.
- Paging's per-gap markers carry a real `data-abs` and explicitly end the
  "non-row children read −1 and sort first" property the anchor searches
  rely on. §3.2's markers-are-never-anchors rule is what makes that safe.
- Paging §7's "anchor preservation across a multi-frame 1000-row prepend"
  is the test §1.4's fixture fix makes falsifiable.
- §5 covers ED3 renderer-side; paging's cache drops need nothing from it,
  because paging's own far-edge eviction exempts every line within
  `prefetchThreshold` of the viewport. If that exemption is ever weakened,
  §5's rule needs the paging call sites added.
- This design is the only source of a per-tab reading position (r1
  finding, claude M5). Paging's `viewportAbs` is per-SURFACE, and with one
  scroller shared across tabs it cannot answer "what was this background
  tab's reader looking at" — which is what a per-tab TTL skip (§5.6) and
  the ack transition's containment both want.

### 7.6 Sequencing, given the work in flight

The seams have landed, so the r1 draft's ordering advice is moot. What
remains:

1. §7.3's `predictReplayJump`/`isFollowing` ordering fix belongs to the
   paging branch and is independent of this design. It is the one item
   here that is a live bug rather than a proposal.
2. `onResumeTransition` wiring (`connection.ts` → `kernel.ts`) is
   outstanding on the paging branch; §7.2's split and §7.3's conservative
   fallback should land with it, while its tests are being written.
3. This design's own work — §3, §4, §5, §6, and §8's fixture change —
   lands independently, touching no file the paging work owns (§9.1).

One ordering constraint remains and it is a testing one: §8's clamping
fixture SHOULD land before or with paging's multi-frame-prepend test,
which is unfalsifiable on the current non-clamping mock.

## 8. Testing

The fixture change is a PREREQUISITE, not a deliverable alongside:

- A shared scroll-element helper whose `scrollTop` setter CLAMPS to
  `[0, scrollHeight − clientHeight]`, used by `scroll.test.ts`,
  `render-read-anchor.test.ts`, and the new tests. Every assertion that
  currently reads `scrollTop === scrollHeight` must be re-expressed
  against the clamped maximum.
- `tabs/index.test.ts` stops mocking the restore as a bare `vi.fn()`.
- An instrumentation assertion for §1.1.1: the saved `{abs, following}`
  pair is observable on every switch, so the §1.3 hypothesis is confirmed
  or refuted against a live session rather than assumed.

New coverage:

- Switch away from a 5000-line tab at the bottom and back: bottom, follow
  ON, asserted across the WHOLE multi-frame drain, not just after frame 1.
- Switch away holding 3000 lines back and return: the anchored LINE
  (`data-abs` at the viewport top) is under the reader when the drain
  settles, follow OFF. Asserted by line, never by pixels.
- The same with the tab grown by 2000 lines while backgrounded — the case
  a pixel offset gets wrong by construction.
- Generation: a second switch mid-drain does not land tab A's anchor into
  tab B's store.
- The settle condition: `renderQueue` reaching zero BETWEEN replay chunks
  does not terminate the restore (the trap `render.ts` documents).
- Cancel: a user gesture mid-drain cancels; the rebuild's OWN clamp does
  NOT (the H1 regression test — red-check by wiring cancel to
  `onScrollPosition`).
- `noteContentShrink`: armed → an announced shrink of any magnitude
  preserves follow; NOT armed when the removal cannot clamp (rows removed
  below the viewport; a wipe whose `scrollHeight` does not fall because of
  the caret overlay) and the next real gesture still registers. This pair
  is the mutant that matters.
- Subpixel: a clamp landing 1.5 px off the bottom under a fractional
  `scrollTop` preserves follow (today's epsilon fails this).
- ED3 stand-down: a reader mid-history, `scrollbackCleared` with a reprint
  at higher indices, and the viewport is NOT moved to the re-resolved row.
  Red-check by removing the adjacency branch.
- A cap trim (not a discard) still re-resolves to the nearest survivor —
  the Safari batch-eviction behavior `render-read-anchor.test.ts` already
  pins must not regress.
- Re-entry to a tab whose browse cache was TTL-dropped while unbound still
  restores (the claude-M4 regression: red-check by adding a
  cancel-on-discard rule).
- `updateFontMetrics`: row height changes enough to move the top row; the
  same `data-abs` is at the viewport top afterwards. A "flush was
  scheduled" assertion is explicitly insufficient.
- Marker-as-anchor: with a gap or trim marker at the viewport top,
  `captureViewMemory` returns the first content row.
- Alt: `captureViewMemory` returns null while alt is active, and a switch
  away from an alt tab and back does not restore an alt-screen row.
- Teardown: `destroy()` during an armed restore leaves no timer that writes
  to a detached element.
- Instrumentation (§10 item 1): the saved `{abs, following}` pair is
  observable on every switch.
- Paging regression (§7.3): a following reader across a predicted replay
  jump drains to 500, not 2500. NOT implemented here — this row exists so
  the paging branch inherits the assertion (§7.3 routing).

## 9. Rollout

- `web-terminal-engine`: `scroll.ts` gains `noteContentShrink(predicted)`
  and deprecates `restoreScrollTop`; no `setFollowing` export.
  `render.ts` gains `captureViewMemory`, `bind`'s `{ view }` option, the
  restore transition and generation, `liveViewportAbs`/`pendingRestoreAbs`,
  the ED3 stand-down, and the `updateFontMetrics` capture-restore.
  `store.ts` is NOT touched. Additive.
- `web-terminal-ui`: the `Tab` record's two scroll fields become one
  `ViewMemory`; `switchTo` calls the atomic `bind(store, { view })` and
  drops its restore rAF. `kernel/types.ts` marks `restoreScrollTop`
  deprecated. Internal to the tabs feature plus a types touch; ui minor.
- Same engine minor as demand-paged scrollback.
- No wire change. No server change (§7.3's fix is client-side and belongs
  to the paging branch).

### 9.1 Working-tree constraint (two agents, one tree)

The paging implementation is in progress in the same working tree, with
substantial uncommitted work in both repos and no branch isolation. At the
time of writing it is active in `web/src/store.ts`, `web/src/intervals.ts`,
`vt/**`, `terminal/**`, `render-golden/**`, and its own new test files
(`store-paging.test.ts`, `intervals.test.ts`, `render-e2e-golden.test.ts`).

This design is scoped to avoid every one of those. `scroll.ts` and
`render.ts` have been quiet for 30+ minutes and are the only engine source
files it edits; the entire ui repo is untouched by the paging work. §5's
withdrawal of the `StoreChanges` change and §3.5's deferral of the
`rebuild()` signature change are both partly motivated by this: they were
the two edits that would have collided.

Consequences an implementer must honor: re-read each file immediately
before editing it, do not commit (the tree contains another agent's
in-progress work), and do not "fix" §7.3 even though it is a real bug — it
is in the file that is hot, and it is the other branch's to fix.

## 10. Resolved questions

The r1 review's open items, now decided:

1. **§1.1.1 instrumentation.** Implement on the code-grounded hypothesis,
   and land the instrumentation in the SAME change (§8). It is a
   log-the-saved-pair assertion, not a gate; if it later shows
   `following: true` on a failing switch, §1.3 reopens with the evidence
   already captured.
2. **§3.4's constants.** Derive from the tabs feature's existing tuned
   values rather than inventing new ones: the 250 ms inbound-quiet settle
   mirrors `CATCHUP_SETTLE_MS`, the absolute bound mirrors
   `CATCHUP_MAX_MS`. Same question, already tuned once, so two constants
   become zero.
3. **§7.3 routing.** Amend `paged-scrollback.md` §5.3 on the PAGING branch,
   where the code lives (§7.3, §9.1). Not touched from here.
4. **§8's clamping fixture.** Lands with this design rather than as its own
   change: this design's tests are the ones that need it, and splitting it
   out would leave a fixture with no consumer.
5. **Alt flip mid-drain.** Cancelling is NOT correct and is not needed —
   §3.4's settle terminates immediately in alt because `rebuild()` queues
   nothing there, and §3.6 keeps an alt measurement out of the saved
   anchor in the first place.

## 11. Review log

- r1 (2026-08, TWO reviewers — the fable stage crashed twice, before
  producing a report; `paged-scrollback.md` §11 records the same recurring
  fable crash at its r8, so this is an environment fault, not a signal
  about the document). Reports: `_adv-scrollfix/report-design-r1-claude.md`
  (604 lines, 4 High / 7 Medium), `report-design-r1-gpt.md` (247 lines,
  6 High / 3 Medium). Both returned NOT-READY-AS-WRITTEN with convergent
  change sets. All four §1 defects were verified as real by both
  reviewers, and claude confirmed every HTML/CSSOM event-loop assumption
  the diagnosis rests on (scroll steps before rAF callbacks; rAF in
  registration order; one scroll event per target per frame).
  Convergent findings, all folded into this revision:
  - The cancel signal fired on the browser's own clamp, so the rebuild
    cancelled its own restore (claude H1, confirmed against
    `scroll.ts:159`). Replaced with self-knowledge (§3.4).
  - `noteContentShrink` could be armed and never consumed, swallowing the
    next real gesture — with the tab-switch wipe as a concrete instance
    via the untouched caret overlay (claude H2, gpt High). Now
    position-gated and self-clearing (§4).
  - §7.3's stale-follow-flag mechanism was REFUTED; `isFollowing` derives
    from `viewportAbs >= win.base` (both reviewers). Rewritten — and
    claude's H3 surfaced a real, confirmed bug in the landed
    `predictReplayJump`/`applyResumeAck` ordering that costs ~2000 lines
    of residency against paging's own budget (§7.3).
  - The bespoke anchor's residual had an inverted sign and mixed
    coordinate spaces (gpt High, claude H4). Replaced by reusing the
    shipped `ReadAnchor` difference form, which deletes most of §3
    (claude M1).
  - `renderQueue.size === 0` is not a terminal signal (gpt High). Settle
    condition added (§3.4).
  - One `viewportAbs` answer for all callers is wrong (gpt High, claude
    M2). Split into live/pending with per-caller rules (§7.2).
  - `updateFontMetrics`'s scheduled flush captures post-change layout
    (gpt High). Now capture-then-restore (§6).
  - `historyDiscarded` as a boolean stood the anchor down on unrelated
    cache pressure (gpt Medium). Replaced with `discardedRanges` in r2,
    then withdrawn entirely in r3 for the renderer-side bound.
  - Public `setFollowing` and immediate `restoreScrollTop` deletion both
    withdrawn (gpt Medium ×2, claude M7): atomic `bind({ view })`, and
    deprecate-then-remove.
  - Blanket-skipping `restoreReadAnchor` while armed reintroduced the
    Safari slide (claude M3); `truncateBelowWindow` was left unclassified
    (claude M6); the design is the only source of a per-tab viewport
    (claude M5) — all three now stated.
  - §7's implementation-state account was two commits stale (both
    reviewers). Re-verified against the tree and rewritten.
  Deferred to §10 rather than resolved: the alt-flip-mid-drain question,
  and whether §1.1.1's instrumentation gates implementation.
- r3 (2026-08, post-review revision, no new review round): folded claude's
  six counter-recommendations and re-scoped against the live working tree
  (§9.1). Changes with design consequence:
  - §5 no longer touches `store.ts` at all. The ED3 bound is observed
    renderer-side in `handleScreen`, which is both collision-free and more
    exact than the r2 draft's `discardedRanges` — and paging's own viewport
    exemption is why browse eviction needs no handling.
  - Cancel-on-region-discard DELETED (claude M4): it would have lost the
    restore on every re-entry to a TTL'd tab, and the row lookup already
    distinguishes the case for free. The cancel set is now two items.
  - §3.5's priority drain DEFERRED (claude counter-rec 3): it changes
    `rebuild()`'s signature and `queueRowsViewportFirst`'s ordering, which
    the in-flight paging branch depends on, to buy ~180 ms in one case that
    `armCatchup` already covers.
  - New §3.6 records six interactions the r2 draft missed: alt screen,
    predictive echo and the IME view's window-relative fallback, text
    selection, the `wt-switching` transform suppressing native scroll
    anchoring, and persistence as an explicit non-goal.
  - Teardown added to §3.4 (claude counter-rec 5): the timer must be
    cleared by `render.init`, `bind`, `scroll.init`, and ui `destroy()`.
  - §7.3's paging bug is now explicitly ROUTED to the paging branch rather
    than carried here (claude counter-rec 1), and §9.1 states why this
    design must not fix it.
  - All five r1 open questions resolved in §10; two constants eliminated by
    reusing the tabs feature's already-tuned `CATCHUP_*` values.
- r4 (2026-08, IMPLEMENTED; status moves to implemented-and-verified). Three
  corrections the code forced, each caught by a test rather than by reading:
  - `noteContentShrink` takes the PRE-mutation offset and arms on OBSERVED
    movement, not a predicted `scrollHeight` (§4). The r3 signature could be
    wrong in both directions; an observed move cannot. Its "cleared at the end
    of the arming flush" rule was also plainly wrong — the clamp's scroll event
    fires on the NEXT frame's scroll steps, after that flush has ended, so
    clearing there would drop the arm before its own event. It is read-and-clear
    per EVENT instead, which bounds the lifetime just as tightly.
  - `restoreReadAnchor` is skipped in the frame the restore LANDS (§3.4). The
    r3 draft asserted the two could not fight because "the anchor's drift
    measures ~0"; the drift is measured against a capture taken BEFORE the
    restore's write, so it measures exactly that write and cancels it. Three of
    the eleven new tests failed on this and passed once the landing frame was
    excluded.
  - The test FIXTURE has to clamp DESTRUCTIVELY. The first version clamped on
    read while retaining the larger offset, so a position "came back" when the
    content grew again — a container that never loses a reading position, which
    is precisely the fiction §1.4 blames for the bug shipping. Fixing the
    fixture is what exposed the read-anchor defect above; the earlier version
    made the restore look like it worked.
  - Also landed: `rowAtViewportTop` unified the two duplicated viewport searches
    (§7.2), and that fixed a pre-existing paging defect on the way — the old
    `viewportAbs` returned the live `window.base` when the viewport top landed
    on the trim marker, so a reader at the very TOP of history reported a
    position ~5000 lines away and the frontier fetch trigger computed from it.
- r5 (2026-08, adversarial review of the IMPLEMENTED CODE; two reviewers, fable
  crashed a third time). Reports: `_adv-scrollfix/report-code-r1-claude.md` (601
  lines, 2 High / 7 Medium, verdict "not safe to ship as-is, but close"),
  `report-code-r1-gpt.md` (286 lines, verdict "not ready as implemented"). Both
  found the state machine itself sound — claude attacked every interleaving in
  the brief and found no sequencing defect — and both found that the two things
  most likely to regress were the two things no test could see. Fixed:
  - **The marker skip was wrong** (claude H1, reproduced): it tested for a
    missing `data-abs`, but `updateGapMarkers` sets one on every gap marker, so
    only the TRIM marker was skipped. A gap marker's index is the first ABSENT
    line, which `rowEls` can never resolve, so a restore anchored there could
    never land and `viewportAbs` could name a line the store does not hold. Now
    decided by identity in `rowEls` (`isContentRow`), in `firstRowAtOrAfter` too.
  - **The ED3 stand-down never fired for the shape it exists for.** The
    condition also required the SURVIVOR to sit at or above the discard base —
    but the reprint re-delivers lines BELOW that base in the same frame, so the
    survivor looked adjacent and the correction fired anyway. The anchor's own
    index being discarded is the whole test. Found only after the test was made
    falsifiable, which took three attempts (below).
  - **The tests could not fail.** `installRowGeometry` derived `offsetTop` from
    `data-abs`, making pixel space and line space ISOMORPHIC, so a pure
    pixel-offset restore passed all 11 tests — including the two whose names
    claim otherwise (claude H2, reproduced by mutation). Document order is now
    the default and `byAbs` is opt-in with the trap documented. Proof: the
    pixel-restore mutant now fails 3 tests. Two of my own assertions were wrong
    under honest geometry and were corrected, not weakened.
  - `updateFontMetrics` could clobber an armed bind restore with a mid-rebuild
    transient (both reviewers); it now refuses to arm over an existing one.
  - Nothing bounded the arm when no flush ever ran (gpt): `pendingRestoreAbs`
    expires it on read as well.
  - `rebuild()`'s wipe — the largest shrink in the system — was UNANNOUNCED, so
    it still relied on the epsilon §4 exists to stop relying on (claude). It now
    announces.
  - An explicitly NULL view (a first visit, or a tab left on the alternate
    screen) adopted no follow state at all, so those tabs still inherited the
    outgoing tab's flag — the stale-global-flag bug, narrowed rather than closed
    (gpt). Passing `opts` now always carries a follow intent; null means "no
    memory", which means the tail.
  Not fixed, recorded: `RESTORE_MAX_MS` is 30 s where §3.4 still says 2000 ms
  (the doc is now the stale half); the UI's `^3.7.0` engine floor does not cover
  `captureViewMemory`, so the ui minor needs the floor raised to whichever engine
  release carries it; and the `gen` check is redundant with `bind`/`init` already
  clearing the slot (kept as an invariant guard, not load-bearing).

  NOT DELIVERED, and it matters: §10 item 1's instrumentation. Nothing in the
  shipped code records the saved `{abs, following}` pair, so §1.1.1's admission
  still stands — the fix is complete for a tab left HOLDING (which includes
  §1.3's "holding at the bottom"), and for a tab left genuinely FOLLOWING the
  per-frame pin already recovered before this change. If the user's failing
  switch was on a tab whose flag was really `following: true`, symptom A has a
  cause this design has not found, and the diagnosis reopens. One observation
  settles it: the value of `Tab.view.following` on a switch that lands wrong.
  Until then this is the one claim here resting on inference rather than a test.
- r6 (2026-08, closing the two review items that were left open). §7.3's paging
  defect is now FIXED and pinned, in `store.ts`, once that file had been quiet
  for half an hour: `applyResumeAck` captures `isFollowing(viewportAbs)` BEFORE
  step 4 can retire the window descriptor, and step 5's containment test reads
  the captured value. `store-resume-containment.test.ts` pins both directions
  (a following reader drains to the small target; a reader parked inside the band
  keeps the full cache) and red-checks: reverting the fix produces 2500 retained
  where the design budgets ~500.

  Two notes for whoever owns `paged-scrollback.md`. The band is
  `[oldest, sentHaveThrough + 1)` and the real client sends its HIGHEST index
  (`getHaveThrough` -> `render.getHighestIndex`), which is the window's bottom
  row — so the band spans the old window rows, which is what makes a following
  reader land inside it. A first attempt at this test used a `sentHaveThrough`
  below the window base, the band then excluded the reader, and the test passed
  against the bug. And §5.3's worked example ("keeping the newest 500") is
  approximate: the viewport exemption keeps `prefetchThreshold` lines either side
  of the reader, so the honest ceiling is the `2 * prefetchThreshold + 1`
  invariant §5.3 already states, ~504 in practice.

  Still open, deliberately: the UI's `^3.7.0` engine floor. Engine tags stop at
  v3.6.0 and `paged-scrollback.md` targets "the minor after v3.7.0", so if both
  changes ride that minor the floor must name it (3.8.0) — `^3.7.0` would let a
  consumer resolve an engine without `captureViewMemory`. Left as a
  release-coordination decision rather than a guessed version bump.
- r7 (2026-08, second review round, scoped to r5/r6's own fixes — the part no one
  had reviewed). Two reviewers again; both mutation-tested independently rather
  than trusting the red-checks, which was the right call. Reports:
  `_adv-scrollfix/report-code-r2-claude.md` (646 lines, 15 mutants),
  `report-code-r2-gpt.md`. Nine of the fifteen mutants were killed, confirming
  the marker identity rule, both ED3 forms, the pixel-restore kill under the new
  fixture default, `bind`'s synchronous follow adoption, the `lastWrote` gesture
  check, and the conditional read-anchor. Fixed here:
  - **`store-resume-containment.test.ts`'s second test was VACUOUS** — a
    hard-coded `inside = false`, the exact failure it names, passed it. The
    viewport exemption keeps ~1001 lines whatever the target, so every range
    assertion held for the wrong behavior. Measured the real numbers (correct
    2500, wrong 2304) and asserted the CAP exactly, which is §5.3's actual
    contract. Both tests now kill their mutants. This was the third red-check in
    this workstream that passed against its own bug; the lesson is recorded in the
    test itself.
  - **A real bug in r5's `updateFontMetrics` guard**: refusing to arm over an
    existing restore left that restore's `lastWrote` stale, so the reflow's own
    clamp read as a gesture on the next flush and cancelled the arm it was
    protecting. It now refreshes the baseline first.
  - The marker-walk comment claimed a bound of one adjacent marker. Measured
    false: two gap markers can be adjacent, and can be the last two children,
    once `predictReplayJump` retires the window. Corrected to O(gaps + 1).
  - New tests for fix 7's null-view branch (kills its mutant) and for
    `firstRowAtOrAfter`'s marker skip.
  Honestly still UNCOVERED, and stated rather than papered over: the narrowing
  half of the ED3 condition (`anchor.abs <`), and `firstRowAtOrAfter`'s marker
  walk — the latter is defensive rather than load-bearing: eviction is
  oldest-first, so a surviving row in the lower run always precedes the marker,
  and no reachable case was constructible.
- r8 (2026-08, closing r7's own leftovers). Everything r7 left uncovered that
  could be covered now is, and the two that could not are argued rather than
  asserted:
  - **The fixture's row height is now MUTABLE** (`RowGeometry.setRowHeight`),
    which is what unblocked the `updateFontMetrics` test r7 called impossible.
    Two false starts on the way, both instructive: growing the rows makes the
    container TALLER so nothing clamps and the hazard never arises, and at frame
    one of a rebuild `scrollTop` is still 0 so a shrink cannot clamp it either.
    The state the bug needs is "armed AND at a non-zero offset", which needs a
    backlog deep enough that the anchored line stays unbuilt while the read
    anchor's per-frame corrections move the viewport off zero. With that, the
    mutant is vivid: dropping the baseline refresh strands the reader on line
    2983 instead of their line 100.
  - **`pendingRestoreAbs`'s read expiry** is now pinned with a clock advance and
    no intervening flush.
  - **The shrink announcement is now GATED on the pass having actually removed
    rows**, and that is a soundness fix, not just coverage. Announcing after
    every flush is unsound on Safari, which updates `scrollTop` past the maximum
    during an overscroll bounce: the settle back is a downward move in value with
    no content change at all, so the arm would have swallowed the user's own drag.
    A pass that removed nothing cannot have clamped. Every removal site sets the
    flag (full reset, cap eviction, alt exit, the stale-row path in `upsertRow`),
    and `rebuild`'s wipe keeps its own announcement. Pinned in both directions:
    an add-only flush must not announce, an evicting flush must.
  - Writing that test caught a THIRD vacuous test of my own: it mutated the store
    directly, which schedules no flush, so both halves were trivially true.
  Still uncovered, with the argument rather than a promise to fix: the narrowing
  half of the ED3 condition needs an anchor at or above the discard base whose row
  is ALSO cap-trimmed in the same pass, and cap eviction is oldest-first, so the
  setup is contrived enough that the guard is cheaper than the test.
  `firstRowAtOrAfter`'s walk stays for symmetry with `rowAtViewportTop`; no
  reachable case puts a marker there.

  The UI's engine floor is now `^3.8.0` in BOTH `package.json` and `jsr.json`,
  on the only self-consistent reading (`paged-scrollback.md` targets "the minor
  after v3.7.0", and this design rides that same minor). Confirm at release: if
  these APIs actually ship in 3.7.0, the floor is one minor too high.
