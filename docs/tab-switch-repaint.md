# Tab-switch repaint: what to change, and what to measure first

**Status:** r3. r1 refuted the first draft's central proposal; r2 found that two
of the four changes the r2 draft called safe were not. Both rounds are applied.
Section 3 is implementable. Section 4 is harmless tidy-up. Section 5 is blocked
on measurement and must not be built blind.

**Surface:** `web-terminal-engine/web/src/render.ts`;
`web-terminal-ui/css/{40-animations,01-scope}.css`,
`web-terminal-ui/src/kernel/kernel.ts`,
`web-terminal-ui/src/features/tabs/index.ts`.

**Siblings:** `docs/scroll-position-fidelity.md` (the reading position across a
switch, and the deferred §3.5 drain seeding that C3 depends on),
`docs/paged-scrollback.md` (the residency model).

**Constraint from the requester:** no degradation and no animation skip. The
animation runs on every switch, for every tab size, with the same duration,
easing and perceived result. Nothing in section 3 or 4 changes what the user
sees. Every item that does is in section 5, priced, and not adopted.

---

## 1. The report

Switching tabs in the consumer app sometimes shows a full black terminal. One
tick of the scroll wheel reveals the content. Frequent, not systematic. A scroll
wheel means a desktop engine. **The engine and version are not pinned, and
desktop WebKit has not been excluded** (raised by two reviewers in both rounds).

## 2. What is known, and what is not

**Known, from the code.** `switchTo` calls `render.bind`
(`features/tabs/index.ts:1561`), which calls `rebuild` (`render.ts:489`), which
runs `output.replaceChildren()` synchronously (`render.ts:495`) and builds zero
rows. Rows are built only inside `requestAnimationFrame`
(`render.ts:955-959`), at most 300 per frame (`render.ts:169`), ordered
viewport-first by the flush-time split at `render.ts:1353-1363`. A 5000-line tab
needs about 17 frames. One frame after the bind, `flashSwitch`
(`features/tabs/index.ts:1672`) starts a 200 ms animation on `.term-output`
(`css/40-animations.css:110-140`, `--dur-standard: 0.2s` at
`css/00-tokens.css:175`) and schedules class removal on a 360 ms `setTimeout`
(`features/tabs/index.ts:1682`). Because the class is added inside a rAF, the
class outlives the animation by roughly 144 ms rather than 160 ms, and the
animated element is mutated on every frame of the animation's 200 ms.

**Known, from r1.** The first draft blamed a tile or layer-size budget on an
85,000 px element. Not supported:

- Chromium's `cc` rasterises tiles near the viewport, not whole layers, and
  `.term` already clips with `overflow: hidden auto`
  (`css/02-terminal.css:10`), so the effect surface is already pane-sized. Only
  the layer's declared bounds are tall.
- Gecko applies its size restriction to transform animations only, not opacity,
  and its failure mode there is a fall back to a main-thread animation rather
  than a lost paint (Bugzilla 1100357).
- No reviewer, in either round, found an upstream engine bug matching the
  signature, open or fixed.

**Field evidence, reported while the code review was running.** Recorded verbatim
because the third walks the first one back, and a symptom the reporter is unsure
about is still evidence about duration and about what the settled state is not.

- **O1.** On the black screen, sometimes the bottom of some text is visible at the
  TOP of the viewport. Read at the time as "history painted, live view did not".
- **O2.** It happens after not having visited the affected tab for a while.
- **O3.** Uncertain about O1. The cut-off text at the top does not stay; when the
  view settles it is gone, "pushed up or replaced", and it is NOT what ends up at
  the bottom of the settled live view. Visible for about a second, too briefly to
  read.

**These are a geometry signature, not a paint signature, and that moves the
diagnosis.** A lost tile, a stale surface or a missed presentation blanks a pane
uniformly; it does not leave the last sliver of built content clipped to the top
edge, and it does not resolve itself in about a second with no input. A partial row
at the top with emptiness below, for a bounded interval, is what `scrollTop` sitting
at or just past the end of the BUILT content looks like while `scrollHeight` extends
beyond it. So the weight moves off section 5 (both of whose candidates are about the
animated layer) and onto section 6 (the wipe's geometry and the content-space
overlays). O2 fits the same reading: a tab away for a while has more to rebuild, and
its saved reading position is likelier to name a line the store no longer holds.

**Two mechanisms of that shape have now been tested and did NOT reproduce.** The
harness gained a second predicate for exactly this fault class, `parkedInVoid`
(`scrollTop` past the bottom of the last built row), because the original
`covered > 3` predicate is blind to it by construction: when the reader is past the
built content, `covered` is 0 or 1, so no number of samples under the first
predicate could ever report this fault. Zero void frames in the ordinary switch
loop. A dedicated aged-tab scenario then reproduced O2's setup directly — park the
reader deep in history, leave, push 4000 lines into the background tab's store so
the saved position is evicted, return — over six rounds and 48 samples: zero void
frames, and a full viewport of rows under the reader every time. So the
restore-cannot-land mechanism is not it, at least on the eviction path. The paging
path is still untested here, because the harness has no paging capability to drop a
browse cache from.

**The one plausible aggravator that survived both rounds:** an animation runs on
a subtree mutated on every frame of its 200 ms, and the class that ends it is
removed on a timer unrelated to either the animation or the drain. The second
half is a defect on its own terms; section 3 fixes it. The first half is C3, and
r2 showed it is not safe to fix yet.

---

## 3. Implement: two changes, no visible effect

### 3.1 A scroll drains a non-empty queue

The renderer exposes ONE hook for a scroll-position event,
`render.handleScrollPosition()`, and the consumer wires it to
`scroll.init({ onScrollPosition })` (`kernel.ts`). It does two things in order:
re-evaluate the demand-paging trigger, then resume a drain that still has rows
queued.

**One hook, not two exported calls.** The first implementation exported
`resumeDrain` beside the existing `maybeFetchHistory` and left the consumer to
call both. That is the silent-omission trap the paged-scrollback docs already warn
about: a consumer upgrading the engine and keeping its one-line wiring would get
paging and no drain recovery, with no type error and no runtime complaint. The
engine cannot register the wiring itself (`scroll.ts` cannot import `render`
without a cycle), so the next best thing is a single hook that cannot be
half-wired. `maybeFetchHistory` stays exported for the transport's own retry path,
which is a different event.

**Three refusals inside the resume, each load-bearing.**

- **An empty queue.** `flushRender` runs `applyPendingRestore`,
  `restoreReadAnchor` and `stickToBottomIfFollowing` unconditionally; only the
  drain is queue-gated. Since `atBottom()` engages follow anywhere within
  `BOTTOM_TOLERANCE_PX = 24` (`scroll.ts:90`, `scroll.ts:120-121`) while
  `stickToBottom` pins whenever `distanceFromBottom() > 0`
  (`scroll.ts:289-296`), an ungated version would make a downward scroll landing
  1 to 24 px short of the tail snap to the bottom one frame later, on an idle tab
  where nothing schedules a flush today. It would also expire an armed restore
  early through the 1 px foreign-write detector.
- **Alt screen.** Alt is a NAMED suspension (4.1), and a resumption path that
  ignores it is not a resumption path. The drain would survive anyway, because the
  flush's alt branch returns before it, but the flush around it would still call
  `renderAlt` (which replaces the whole output subtree, destroying an in-alt
  selection) and run all three position invariants. Leaning on the body of a
  function to make a scheduling decision safe is how the next edit to that body
  becomes a bug.
- **A give-up already retried.** The catch's give-up exists so a deterministically
  throwing row cannot become a 60 fps loop. An unbounded scroll-driven retry moves
  that loop outside the module instead of removing it: a flick is one position
  event per frame, and the position callback fires for the browser's own clamps
  too. One retry per give-up serves the case the hook exists for, and the retry is
  re-earned only by real forward progress or an empty queue, never by the give-up
  log itself.

**What it fixes.** A partially built surface currently cannot be completed by the
user's own recovery action. It also closes the residual gap in 4.1.

**Note the honest scope.** An earlier draft said a scroll "triggers no rendering
at all". Too strong: a scroll already emits `scroll:state`, which mutates chrome,
and already reaches `scheduleFlush` indirectly when `maybeFetchHistory` produces a
history reply. The accurate claim is that a scroll cannot drain rows the store
already holds.

**Ordering constraint.** Land this after the sampling in section 7. It creates a
third path by which a scroll can make content appear, which is the discriminator
step 7.3 depends on.

### 3.2 The animation's end drives the class removal

Remove the `wt-switching*` classes on `animationend`, keep the timer as a net, and
cancel the timer on the event.

**The original justification for this section was wrong, and it is worth recording
why rather than quietly restating the change.** The first draft argued from two
numbers that must agree and do not: the CSS duration is 0.2 s and the JS timeout is
360 ms, so the class outlives the animation by about 144 ms, which on a tab above
about 3000 rows lands mid-drain. Verified since, and the 144 ms is **inert as far as
style goes**: only the three animation rules in `40-animations.css` read
`wt-switching*`, the keyframes are `from`-only, and the `animation` shorthand leaves
`animation-fill-mode: none`, so once the animation finishes nothing it carries
applies. A class sitting there for another 144 ms changes no computed style. Whether
an applied-but-finished `animation` keeps a composited layer alive in Blink is a
separate question this document cannot answer without a GPU and a Layers-panel
trace; if it does, the original argument is retroactively right, and if it does not,
the 144 ms cost nothing.

**What justifies the section anyway** are two defects that do not depend on that
question, one of which predates the change:

- **Two switches inside one frame ran two animations at once.** Both class-add
  callbacks were pending, both landed, both classes applied. Cancelling the pending
  frame in the shared teardown fixes it.
- **The net could cancel the animation before it started.** The 360 ms timer was
  armed beside the class-add frame rather than inside it. rAF is paused in a hidden
  document and deferred under a blocked main thread, so the timer could fire first
  and its cleanup would cancel the pending class-add, skipping the animation
  outright. That is the no-skip constraint being broken by the fix meant to respect
  it.

Reading the event rather than the timer is then the right shape on its own terms
(one signal, not two numbers that must agree), and it costs one filtered listener.

**The three keyframe names the listener matches are a cross-language contract**, so
they live in one internal module (`features/tabs/switch-anim.ts`) and
`css-contract.test.ts` asserts the CSS agrees. Without that test a rename on either
side is silent in the worst way: the listener stops matching, the class comes off on
the fallback timer instead, and every behavioural test still passes because the
tests dispatch the JS constant.

Five implementation requirements, all from review, and the change is wrong without
them:

- **Filter on the ONE expected `animationName`, not the set of three.** An
  animation that completed just as a re-switch landed has its event already queued;
  the old listener is gone by dispatch time, so the NEW listener receives it, and a
  listener accepting all three names lets switch N's completion end switch N+1's
  animation a frame in.
- **Require the class to still be present.** The listener is attached in the
  click's task and the class lands a frame later, so for one frame a matching event
  from anywhere in the subtree would cancel the pending class-add and skip the
  animation outright. An `animationend` cannot precede its own animation, so the
  check is free.
- **Arm the net INSIDE the class-add frame.** Armed beside it, the 360 ms timer is
  a deadline on a request to animate rather than a net around an animation: rAF is
  paused in a hidden document and deferred under a blocked main thread, so the
  timer can fire first and its cleanup cancels the pending class-add. That would
  skip the animation, which the no-skip constraint forbids.
- **Keep the timer.** `animationend` does not fire when an animation is
  interrupted, `animationcancel` is not reliably delivered in Blink, the
  `animations` feature is optional, and reduced motion removes `.wt-animate`. The
  event is the fast path; the timer is the guarantee.
- **Tear the lifecycle down as one unit** (listener, timer, pending rAF) on
  re-switch and on feature teardown. The pending-rAF half also fixes a defect that
  predates this change: two switches inside one frame left two class-add callbacks,
  so both classes landed and two animations ran at once.

**Two things deliberately NOT done, because their red check could not be made to
fail.** A generation counter was implemented and removed: `endSwitchAnim` removes
the listener synchronously before the next one is added, so a stale listener cannot
receive an event and the counter could never fire. And no event-target check: the
animation is declared on the kernel's `.term-output`, which this feature does not
own, and reaching for it by class name would couple tabs to the kernel's markup.
The residual risk is a consumer applying one of these three library-private
keyframe names to another element inside the surface, and the class check reduces
that to ending a running animation early rather than skipping one.

---

## 4. Harmless tidy-up

None of these would justify a change on its own. All three are safe, and two are
inert by design.

### 4.1 Name the reschedule rule, and assert it once

The reschedule decision has **four** outcomes: the queue is empty, a frame is
scheduled, the drain is deliberately suspended with a named resumption edge (alt,
edge is alt exit), or it stopped and said so (the bounded error path). Anything
else is a stall nobody owns, and `reportUnnamedDrainStall` warns about it.

- **The alt-screen return is correct and must not change.** The invariant is in
  the comment above it (`render.ts:1293-1297`), and it is stronger than an
  optimisation: `renderAlt` runs `output.replaceChildren(...els)`
  (`render.ts:1827`), so continuing the drain during alt would inject
  main-buffer rows into the alt grid.
- **The give-up path keeps its stop and its log.** After
  `MAX_RENDER_NO_PROGRESS_RETRIES = 3` (`render.ts:236`) the catch block in
  `flushRender` stops rescheduling (`render.ts:1197-1203`) and logs it at
  `render.ts:1202`. It is a guard on the process, so it must refuse and report
  rather than loop.

**The canary is once per EPISODE, not once per process.** Its latch clears as soon
as a flush ends in a healthy state, and `init` (the attachment boundary) resets it
with every other piece of pass state. A latch that survived would make the canary
deaf after its first report: silent for every later attachment and for every
genuinely new stall, which defeats the only reason it exists.

**It has a positive test**, which cost some thought. Every named state is
excluded, so the target state is unreachable through the renderer's own paths, and
that is precisely the invariant being asserted. Rather than add a test-only export
to a published API, the test breaks the environment the renderer trusts: a
`requestAnimationFrame` stub that queues the callback but returns no handle, so the
module's record of a scheduled frame is lost while the frame still runs. Without
it, deleting the warn would pass the entire battery.

So this is documentation plus one assertion, not a control-flow rewrite. Its
lasting gain is that a future suspension point has to declare its resumption edge,
which C3 would be.

### 4.2 Close the clip asymmetry

Add `overflow: clip` to `.wt-root.wt-viewport` (`css/01-scope.css:15-18`);
`.wt-root.wt-container` already clips (`css/01-scope.css:20-24`).

**It is inert, and that is why it is here rather than in section 3.** r2 verified
it against every absolutely positioned descendant of the root (the two menus, the
switcher tray, the connection banner, toasts, scroll-to-bottom, the drag preview
clone and the loading overlay), confirmed `overflow: clip` creates no block
formatting context and no scroll container, and noted that the root's box in
viewport mode IS the viewport while `page.css` already hides document overflow.
So nothing can currently paint outside it, and nothing changes. `position: fixed`
descendants escape the clip regardless.

An earlier draft called this a prerequisite for C1. It is not: the viewport
already bounds what C1 could reveal.

### 4.3 Correct the stale prose

`scroll.ts`'s comment on `noteContentShrink` names "a wipe whose `scrollHeight`
is held up by an absolutely-positioned overlay" while listing the removals its
arm must not predict. In this code that case is live rather than illustrative:
the caret overlay, the predicted cursor, the IME view and the hidden textarea all
carry a `top` in content coordinates, and `rebuild` does not reset them, while
`init` does (`render.ts:326-333`). Say so, and point at 6.2.

---

## 5. Blocked on measurement

Do not build any of these without the evidence in section 7.

### 5.1 C1: move the animation off the scrolled content

Change the animation selectors from `.term.wt-switching .term-output` to
`.term.wt-switching` and the two directional variants. `.term` is
`position: absolute; inset: 0` (`css/02-terminal.css:8-9`), so its box is the
pane and its height never changes during the drain: a height-stable, pane-sized
promoted layer for both paths, from one rule. This is r1's counter-proposal, and
it replaced the first draft's veil, wrapper and composite algebra.

**Two visible differences, both needing approval under the constraint.** The
three overlays inside `.term` would fade with the content: the caret
(`z-index: 3`), the predicted cursor (`z-index: 4`) and the IME view
(`z-index: 5`). And the scrollbar is part of `.term`'s box, so on the slide path
it slides with the pane, which is visible with `scrollbar-gutter: stable` and the
boundary's thin scrollbars. 6.2 removes the first difference.

**And it buys less than the first draft claimed.** The render surface is already
pane-sized (section 2), so C1 changes the layer's declared bounds and its height
stability, not the volume of raster. If the fault is tiles that were not ready,
C1 does nothing.

**Narrower variant if the scrollbar cost is refused:** apply only the opacity
half to `.term` and leave the transform on `.term-output`, as two animations with
the same duration and easing. The slide is then identical to today and only the
overlay fade differs. It gives up the height-stable layer on the slide path,
which is most switches, because `switchTo` derives a direction from the index
delta and so almost every switch slides.

**Its guard, if adopted.** A narrow contract test: no rule in the bundle may put
`animation`, `transform`, `opacity`, `will-change` or `filter` on a selector
ending in `.term-output`. **This test cannot be written before C1**, because the
only three selectors in the bundle ending in `.term-output` are the switch rules
themselves (`css/40-animations.css:110-120`) and each declares `animation`, so
the rule is currently the negation of shipping CSS. An earlier draft called it
worth having either way; it is not. Note also that the rule assertions in
`css-contract.test.ts` read seven named files (`css-contract.test.ts:18-24`)
while `readBundle()` (`css-contract.test.ts:346`) feeds only the token
inventory, so the new assertion must use the former and must include
`40-animations.css`.

### 5.2 C3: hold the below-window drain during the animation

Hold the below-window half of the drain while the animation runs, so the animated
subtree is not mutated 16 times inside its 200 ms. **This targets the one
surviving aggravator, and r2 moved it out of the implement set. Five reasons:**

1. **Its own precondition is deferred elsewhere.** A tab left scrolled deep
   anchors its restore on a row the held half would build, so the hold needs the
   §3.5 drain seeding from `scroll-position-fidelity.md`. That sibling
   explicitly defers §3.5 with "measure, then decide", partly because it changes
   `rebuild()`'s signature and the drain ordering an in-flight paging branch
   depends on. So this is gated on another document's open decision.
2. **It can evict around the wrong centre.** `viewportAbs()` resolves the
   reader's position from built DOM (`render.ts:1546-1553`) and feeds the store's
   browse-cache eviction exemption (`store.ts:852`, `store.ts:925-927`). Under
   the hold, a scrolled-up reader's `viewportAbs()` collapses toward the window
   base, so a history reply arriving inside the hold evicts around the wrong
   point.
3. **It creates the spin this document rejects elsewhere.** A hold makes
   `renderQueue.size > 0` permanently true, so the end-of-body reschedule
   (`render.ts:1395`) becomes a per-frame no-op loop, which is exactly the
   objection 4.1 accepts for the alt branch. It would have to be the third
   outcome of 4.1's rule, with the animation's end as its resumption edge.
4. **Its window is undefined when no animation runs.** With `.wt-animate` absent
   or reduced motion set, no animation fires, so the window degrades to the
   360 ms timer and reduced-motion users pay the longest hold for no animation.
5. **It is not invisible.** The catch-up cue
   (`CATCHUP_MIN_BACKLOG = 400`, `features/tabs/index.ts:184`) stays up longer,
   up to 360 ms on the no-event path; `captureViewMemory`
   (`features/tabs/index.ts:1509`) can overwrite a deep saved position during the
   widened window; and a live window taller than the built band is not covered by
   "below-window equals offscreen".

**A strictly cheaper competitor, which r2 says was dropped and should not have
been:** change only the ORDER, not the budget. Start the unchanged animation
after the first visible-row commit rather than one rAF after the bind. That
removes the overlap between the animation and the bulk of the mutation without a
hold, without a new flag, without touching the drain, and without any of the five
problems above. It is the smallest experiment against the same hypothesis, and it
should be measured before C3.

### 5.3 C2: the programmatic nudge (rejected, recorded)

Write `+1` then `-1` to `scrollTop` across two frames through `scroll.ts` so the
follow state survives. Two writes in one task coalesce into one scroll event and
a net-zero move invalidates nothing, so the two-frame shape is the only one that
could work. `preserveFollowOnce` (`scroll.ts:104`) suffices as a boolean, because
one scroll event is dispatched per frame per scroller.

Rejected: it treats a symptom, taxes every switch with a frame of work and a 1 px
jitter, and bets on compositor behaviour no test can pin.

### 5.4 Recorded alternative: bound paint instead of moving the animation

`content-visibility: auto` bounds paint while keeping content selectable and
findable, which would attack the tall-subtree question from the other side. The
caveat is decisive and belongs with it: it enables exactly the containments
`css/02-terminal.css:69-78` forbids over a WebKit paint-suppression bug that
made the inverse cursor span stick at its old column. So it needs its own WebKit
measurement before it can be considered at all. Dropped from r2 by accident;
recorded here so it is not re-derived.

---

## 6. Structural notes, nothing proposed

### 6.1 The wipe generates the restore machinery

`bind` destroys the DOM and rebuilds it over about 17 frames. `ViewMemory`,
`pendingRestore`, the 1 px foreign-write detector, `noteContentShrink`,
`restoreReadAnchor` and the follow-flag adoption inside `bind` all exist to
survive that wipe. The absolute-index model was introduced in this engine to
delete a reconciliation layer; the switch path reintroduced one at the DOM level.

**r1 corrected the framing this rests on.** The way out is not blocked by a
choice between native selection and bounded DOM. hterm virtualises rows and keeps
native selection through an on-demand row provider, and `css/02-terminal.css:132`
names hterm as the source of this codebase's own selection pattern. xterm.js took
the other road and pays with its own selection implementation and a separate
accessibility tree. So virtualisation costs selection **across rows that are not
rendered**, not selection as such, and a select-all or copy-buffer affordance
covers that. The resident budget is also larger than first stated, up to 4000
rows (a 1500-row tail plus `BROWSE_CACHE_CAP = 2500`, `store.ts:192`), and a
detached subtree has no compositor cost. Both corrections strengthen the case for
bounded DOM.

### 6.2 Overlays out of content space

The caret overlay, the predicted cursor and the IME view sit inside the scroll
container with a `top` in content coordinates, recomputed per flush. A stale one
holds `scrollHeight` above the built content (4.3), and their place in the stack
is what makes C1's overlay fade unavoidable. Only the hidden textarea has a
reason to stay, because iOS places the soft keyboard relative to the focused
input. Moving the three read-only overlays into pane space needs one measurement:
the caret must not visibly lag a fast scroll.

### 6.3 A scroll controller per tab

`scroll.ts` holds one `following` flag, one `scrollEl`, one `lastScrollTop`, one
`preserveFollowOnce` and one `shrinkArmed` for N tabs, and mutates them on switch
through `restoreView`. `web-terminal-ui`'s own rule says per-tab in-progress
state gets one instance per tab, never one shared instance reset on switch.
`LineStore` obeys it. The adopt-on-bind seam is the patch for that violation.

### 6.4 One socket, many sessions

`connection.setSession` calls `reconnectNow`, so every switch pays a handshake
and a resume before the first live frame and no background tab is ever warm.
Carrying the session id per frame would make a switch a subscribe message. It is
a wire change, and a warm background session costs bandwidth for output nobody
reads, so it is a decision, not an obvious win.

---

## 7. Measure first

Section 4 needs no evidence. Section 3 needs the sampling below to run first,
because 3.1 changes a discriminator. Section 5 needs all of it.

**Step 1 has been run, and it did NOT reproduce.** A harness that bundles the real
UI and engine with the real stylesheet, fakes only the transport, and pushes
content straight into the bound store measured 12 switches and 96 pixel samples in
each of two launch configurations: zero frames where the geometry said rows covered
the viewport and the pixels said otherwise. Ink never fell below 3.9% of the probe.
One to three samples per switch landed while a `wt-switching*` class was on
`.term`, so the animation window was sampled rather than missed, and between 16 and
19 samples per run were discarded because the geometry moved under the camera.
Five caveats bound that result, and the last two are why it is weak rather than
dispositive: the container has no GPU (`--enable-unsafe-swiftshader`), so
compositing is software; each screenshot costs about 85 ms, so a single-frame miss
can hide between samples; the workload is faked too, since the silent socket never
produces the resume traffic a real switch's reconnect generates; **a CDP screenshot
requests a compositing pass rather than reading an already-presented frame, so a
lost-invalidation or stale-surface fault is invisible to this instrument by
construction**; and `CDPScreenshotNewSurface` chooses which surface is rendered
into rather than whether rendering happens, so removing it (also clean) is not the
explanation. Harness, method and caveats: `_adv-switchpaint/harness/`.

So section 5 stays blocked, and the remaining steps are the ones that need real
hardware.

1. **Reproduce and sample.** Done in software rendering, negative. Re-run on a
   GPU-backed headed browser, on the reporter's engine and version.
2. **Pin the engine and version, including desktop WebKit.** The candidate
   mechanisms differ per engine (section 2), and no round has excluded WebKit.
3. **Separate the three scroll-triggered recoveries.** A repaint is only one of
   them: a scroll can repaint within one round trip through the paging path, and
   a downward tick can re-engage follow so the next flush pins the viewport. The
   harness implements this discriminator (`scrollTop` and the row count either
   side of the tick) and has had nothing to run it on. Do this before landing 3.1.
4. **Discriminate lost invalidation from tiles-not-ready.** If it is the second,
   the ordering change in 5.2 is the candidate and C1 does nothing. A rendering
   forced to a known colour profile, plus frame-ordering timestamps around the
   bind, the first visible commit and the class add, separates them.
5. **Assert the recovery in the harness, not by eye.** Implemented: the fault is
   a predicate over geometry against pixels, not a screenshot a human compares.
6. **Then measure, cheapest first:** the 5.2 ordering change, then C1, then C3.

Guard rails: the engine and UI batteries, and the existing display-conformance
tiers.

---

## 8. Open questions

1. Is the premise real? Step 7.1 can refute it. Three reviewers called it
   unproven at r1 and none withdrew that at r2.
2. Which engine and version, and is it WebKit?
3. Lost invalidation or tiles not ready?
4. Is C1's overlay fade acceptable, or must 6.2 land first?
5. What does a held below-window queue report as the store's frontier, given
   `maybeFetchHistory` runs after every flush and on every scroll? This gates
   C3, and it is the question that moved C3 out of the implement set.
6. Does a repositioned `role="status"` gap marker re-announce in real assistive
   technology?

---

## 9. Review log

### r1 (Claude, GPT, Fable, against the first draft)

All three returned `premise-unproven`. Applied: the tile-budget mechanism is
unsupported; the veil, the `.term-pane` wrapper and the composite algebra were
**deleted** (the veil had used 0.3 for a keyframe that starts at 0.55, and
outside `.term` it would have missed the DECSCNM reverse-video override at
`css/02-terminal.css:325-328` and faded black over a light screen); the
two-declaration counter-proposal replaced them as C1; the alt-screen return is
correct by a documented invariant and the give-up path does log;
`animationend` cannot own the transition alone; a text CSS test cannot express
the general invariant; hterm virtualises and keeps native selection; the
resident budget is up to 4000 rows; the nudge needs no counter; the
subpixel-antialiasing question was moot; almost every switch slides rather than
fades.

### r2 (same three, against the simplified draft)

Two returned `part-a-not-safe`, one `ready-with-changes`. All three agreed the
r2 "implement now" set was wrong. Applied:

- **A1 was not invisible.** `flushRender` runs the three position invariants
  unconditionally, and `atBottom()` tolerates 24 px, so an ungated scroll flush
  would snap a near-tail scroll to the bottom on an idle tab. Now gated on a
  non-empty queue (3.1), and ordered after the sampling because it adds a third
  scroll-triggered recovery.
- **A3 was misclassified.** Moved to C3 (5.2) with all five reasons recorded,
  and its cheaper competitor (change the animation's start ORDER, not the drain
  budget) restored from a dropped r1 finding and put ahead of it.
- **A4 is inert**, so it moved to the tidy-up section, and it is not a
  prerequisite for C1.
- **B2 was not harmless.** The only three selectors ending in `.term-output` are
  the switch rules and each declares `animation`, so the test negates shipping
  CSS. It is now C1's guard, not a standing item, and its mechanism is corrected
  (rule assertions read seven named files; `readBundle()` feeds only the token
  inventory).
- **A2 needs an `animationName` and target filter plus a listener lifecycle**,
  and the no-animation case needs the timer. All four requirements are now
  stated.
- **Restored from dropped r1 findings:** `content-visibility: auto` with its
  WebKit blocker (5.4); the ordering-only experiment (5.2); the
  colour-profile and frame-ordering discriminators and the runtime paint
  assertion (7.4, 7.5); desktop WebKit as an unexcluded engine (1, 7.2).
- **Citations corrected:** `--dur-standard` is at `css/00-tokens.css:175`, not
  169; the overlay reset is `render.ts:326-333`, not 334; per-frame
  viewport-first ordering comes from the flush-time split at
  `render.ts:1353-1363`; the class outlives the animation by roughly 144 ms, not
  160 ms, because it is added inside a rAF; the "a scroll triggers no rendering
  at all" overstatement is corrected in 3.1; 4.3 no longer calls the `scroll.ts`
  comment hypothetical; the section numbering hole is closed.

Reports: `_adv-switchpaint/report-design-r{1,2}-{claude,gpt,fable}.md`.

### r3-code (Claude, GPT, Fable, against the implementation of sections 3 and 4)

Two `ship-with-changes` and one `part-a-not-safe`-equivalent. Every finding below
is applied, and the code was re-verified and red-checked after.

- **Both alt-screen tests were vacuous, and worse than either reviewer found.**
  They used `alt: true` on the screen message; the wire field is `altActive`, so
  the store never entered alt and every alt assertion ran against a main-screen
  store. Rewritten to enter alt through the flush (`rebuild` clears the queue when
  the store is already alt, so it cannot build the suspended state), and paired
  with an alt-exit test so the refusal is a suspension rather than a leak.
- **The resume ignored the named alt suspension**, surviving only because the
  flush's alt branch returns early. Now refused by its own guard.
- **A scroll-driven retry reopened the bounded give-up loop.** A flick is one
  position event per frame, so a permanently stuck row would log two errors per
  frame for the length of a gesture. Now one retry per give-up, re-earned only by
  forward progress or an empty queue.
- **The stall canary was once per process.** `init` did not reset it, so one report
  made every later attachment and every genuinely new stall silent. Now once per
  episode, plus the `init` reset, plus a positive test (previously the warn was
  dead code as far as the battery was concerned).
- **The `animationend` filter accepted any of the three names.** A completed
  animation's queued event can reach the next switch's listener, so switch N could
  end switch N+1. Now the one expected name, plus a class-present check that closes
  the pre-first-frame window. A generation counter was implemented and removed when
  its red check could not be made to fail.
- **The net could cancel the animation before it started.** The 360 ms timer was
  armed beside the class-add frame rather than inside it, so a hidden document or a
  blocked main thread could let it fire first and skip the animation. Now armed in
  the frame that adds the class.
- **Two exported calls became one hook.** `resumeDrain` was public and the consumer
  had to remember it beside `maybeFetchHistory`. Now `render.handleScrollPosition`,
  and the engine README's wiring snippet is updated in the same change.
- **The kernel's half shipped dark.** The seam test asserted the callback was a
  function and never invoked it. It now calls it and asserts the renderer is
  reached.
- **The harness overstated itself.** A fixed report filename let the second run
  destroy the first run's evidence, so a claim of 24 switches rested on an artifact
  holding 12. Per-configuration filenames now, geometry read before AND after each
  screenshot with unstable samples discarded (16 to 19 per run, each a candidate
  false positive before), and two caveats added: the faked workload, and that a CDP
  screenshot cannot observe a presentation failure by construction.
- **UI test fidelity.** Events now dispatch from `.term-output` and bubble as they
  do in the browser; the "unrelated animation" names are real bundle names
  (`wt-tab-in`, `wt-blink-anim`) rather than two that do not exist; rAF handles are
  monotonic with real cancellation via a Map; and the missing orderings are covered
  (two switches before one frame, destroy before the pending frame, a matching event
  before the class lands, a post-teardown event).
- **`scroll.ts`'s corrected comment over-claimed.** Renderer `init` drops only the
  two overlays it owns; a consumer remount recreates the other two. Narrowed, with
  the §6.2 pointer the doc asked for.

Reports: `_adv-switchpaint/report-code-r1-{claude,gpt,fable}.md`.

Note on process: the Claude stage of this round died seven seconds after dispatch
with nothing written, while the stage that appeared hung had in fact finished its
report 26 minutes earlier. A dispatch return says nothing about whether the work
landed; check the disk for the artifact first.

### r4-code (Claude, independent, against the corrected implementation)

`do-not-ship` until three lines changed. All applied, re-verified, red-checked.

- **HIGH: the one-retry-per-give-up flag leaked a permanent refusal.** It was reset
  at three of the five sites that reset `renderNoProgressStreak`, and the two misses
  (`rebuild`, and a clean pass) are the common ones. One give-up plus one scroll plus
  any tab switch disabled the resume for the rest of the attachment, invisibly,
  because that state presents to the canary as a plain give-up. The flag is gone: the
  spent retry is carried in the streak itself (`MAX + 1`), so every existing reset
  re-arms it by construction and there is no second variable to keep in sync. New
  test, red-checked by removing `rebuild`'s streak reset.
- **HIGH: the UI's peer floor was not raised.** `handleScrollPosition` ships after
  the engine's current `v3.8.0` tag, so on the declared `^3.7.0` a consumer resolves
  `onScrollPosition: undefined`, which `scroll.init` swallows silently, taking
  demand-paged scrollback down with it. That is a regression, not a missing feature.
  `package.json` and `jsr.json` both moved to `^3.9.0` in the same change, per the
  repo's own precedent.
- **MEDIUM: a stale comment asserted the opposite of the code** and recommended the
  wrong repair. Removed with the flag.
- **MEDIUM, accepted with a note:** `scroll.ts` fires its position seam before its
  announced-shrink early return, so a programmatic clamp can spend the retry with no
  user action. Recorded at the call site; any successful flush re-arms it, and a time
  bound would put a clock on a path whose point is to be cheap.
- **HIGH for the harness's conclusion:** the stability gate discarded the darkest
  samples in both runs, all of them inside the animation window, and the fault
  predicate sits 79x below the observed ink floor and cannot express a partially
  painted pane. Combined with the `covered > 3` blindness above, the honest reading
  is that the harness's negative result bears on "can Chromium rasterise this when
  asked" and on the void class, and on nothing else.
- **MEDIUM, open:** `SWITCH_ANIM`'s three keyframe names duplicate the CSS with no
  check, so a CSS rename would silently fall back to the 360 ms net with every test
  green. Wants a contract assertion.
- **MEDIUM, open, and it challenges 3.2's premise:** the switch keyframes are
  `from`-only and the shorthand leaves `animation-fill-mode: none`, and nothing but
  the three animation rules reads `wt-switching*`, so the class lingering 144 ms past
  the animation may have had no effect at all. If so, 3.2's headline justification is
  wrong even though its two lifecycle fixes (the double-animation bug, and the net
  that could cancel an animation before it started) are real on their own terms. One
  Layers-panel trace on real hardware settles it.
- **LOW, open:** the post-destroy test passes with the listener teardown deleted, so
  that leg of "torn down as one unit" has no red check.
- **Finding 18** is the field-evidence addendum folded into section 2 above, plus one
  item to do regardless of the diagnosis: the background browse-cache sweep passes
  `-1` ("no reader: no position to exempt") when the tabs feature HAS saved that
  tab's reading position, so the sweep can drop the page the saved position names.
  Passing it is one argument. Not done here: it is outside sections 3 and 4 and wants
  its own review.

Reports: `_adv-switchpaint/report-code-r{1,2}-*.md`.
