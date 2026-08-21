# ED3 and the resize redraw: why the engine changes nothing

Status: decided, 2026-08-21. Engine change: none. The defect is upstream.

## The artifact

An inline TUI leaves a frozen copy of its own last frame in scrollback, directly
above the live region, after a resize. Reported against `web-terminal-kiro`, with
kiro-cli as the TUI.

## The engine's behaviour is correct, and stays literal

ED3 (`CSI 3 J`) is "erase saved lines". xterm defines it that way, xterm.js
implements it as clearing everything outside the viewport, and this engine does the
same: `vt/csi.go` raises a flag, `terminal.go` clears the ring and tells clients to
drop history, and `scrollback.Clear()` preserves `committed` so absolute indices
stay monotonic.

Rows that are still ON SCREEN when ED3 arrives are not saved lines. When a later
repaint scrolls them off, they become history at that moment, and committing them
is the correct reading of the sequence. The artifact is the application's frame,
committed because the application asked for it.

**The engine will not infer replacement intent from a redraw.** The precedent
worth citing is kitty, which achieves glitch-free resizing
["by erasing the prompt on resize and allowing the shell to redraw it cleanly"](https://sw.kovidgoyal.net/kitty/shell-integration/)
through shell integration: the application declares intent, the terminal does not
guess. tmux and Alacritty have both discussed the adjacent difficulty of reacting
to SIGWINCH without application cooperation, but those discussions are about shell
reflow rather than a retained ring replayed on reconnect, so they are an analogy
here and not proof. The direct argument is simpler: the engine has no signal that
distinguishes a replacement redraw from genuine output, and inventing one from a
timer would apply to every application on this terminal.

This is a general-purpose terminal. Its documented divergences exist because it
runs over a browser and a network: absolute line indices, a resume protocol,
demand-paged scrollback, a redraw-settle hold. None of them is written for one
consumer, and none of them reinterprets a standard control function. A
resize-triggered ED3 reinterpretation would be the first, it would be
kiro-cli-shaped, and every other application on this terminal would inherit it.

A previous attempt shipped and was reverted (`ce0e90f`, reverted by `eb07d60`); its
two measured defects are recorded in `resume-watermark.md` §9. The reason not to
retry it is the one above, not those defects.

## What we do

1. Patch kiro-cli locally, in the style of the existing `patch-kiro-cli-*` set.
2. File upstream with the root cause, the reproduction and the patch.
3. Change nothing in the engine.

Whether upstream fixes it soon does not affect the engine's position. A conforming
terminal showing a conforming result for a non-conforming byte stream is the
outcome we want, and papering over it here would make this terminal wrong for
every other application in order to flatter one.

## Root cause: candidate, not established

A first attempt named the erase extent in kiro-cli's `writeStaticLines` and was
REFUTED in review. Recorded so it is not re-derived: the branch computes

```js
let i = this.hardwareCursorRow - this.previousViewportTop;
let r = Math.min(i + 1, this.terminal.rows);
```

and that subtraction ALREADY converts the renderer's virtual row into a
viewport-relative row, so `r = i + 1` is designed to reach viewport row 0, with
`\r\x1B[J` clearing from the cursor down. In the valid range the pair covers the
whole viewport and nothing strands. The supporting experiment was tautological: it
drove the erase extent as a free parameter and showed that a short erase strands
rows, which proves nothing about what the application computes.

The corrected candidate, derived from the bookkeeping rather than assumed. The
renderer sets, per frame:

```js
hardwareCursorRow = max(0, Q - 1)                       // CURRENT frame height
maxLinesRendered  = reset ? Q : max(maxLinesRendered, Q) // running MAXIMUM
previousViewportTop = max(0, maxLinesRendered - rows)    // from the maximum
```

`hardwareCursorRow` tracks the current frame while `previousViewportTop` tracks a
historical maximum, so the two leave the same coordinate space as soon as a frame
SHRINKS. Modelled at 6 rows:

| sequence | hcr | maxLines | prevTop | i | r | erase |
| --- | --- | --- | --- | --- | --- | --- |
| one tall frame, Q=10 | 9 | 10 | 4 | 5 | 6 | full viewport |
| Q=20 then Q=8 | 7 | 20 | 14 | -7 | -6 | none at all |
| Q=30 then Q=3 | 2 | 30 | 24 | -22 | -21 | none at all |
| Q=20 then reset Q=8 | 7 | 8 | 2 | 5 | 6 | full viewport |

With `r` non-positive the `for (c = 0; c < r; c++)` loop never runs, so there is no
upward erase and only `\r\x1B[J` executes, from the cursor DOWN. Rows above the
cursor survive, the repaint scrolls them off, and a conforming terminal commits
them. That fits the reported shape: the composer sits at the bottom of a tall
frame, and the frame that follows a completed turn is shorter.

This is a candidate. It has not been observed in a live session, and the first
candidate's failure is the reason to insist on that before acting. Confirming it
needs a PTY capture with the renderer's own `debugLog` output, classifying each
byte burst by call site, because `writeStaticLines` runs when static output is
APPENDED and not on resize at all: the resize handler is a separate path that
writes `CLEAR_ALL`, whose ED2 clears the viewport and strands nothing. The
artifact's reported trigger was a tab switch with no resize, which is consistent
with the static-append path and not with the resize path.

If the candidate holds, the fix is NOT to widen the erase to the full viewport:
that would mask an invariant failure rather than repair it. It is to keep the two
coordinates in one space, so a shrinking frame cannot drive the extent negative.
