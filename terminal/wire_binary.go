// Binary wire format for server → client messages.
//
// Replaces the JSON encoding for screen/scroll/resumeAck so frames stay
// small over slow links (notably iPad on a Korea↔France relay where JSON
// payloads of >100KB caused the browser to choke). The format is little-
// endian fixed-width fields; no length-prefixed dictionary keys, no
// repeated string identifiers.
//
//	[1B] msg_type:    0=screen, 1=scroll, 2=resumeAck, 3=modes, 4=title, 5=pong, 6=clipboard, 7=ackOnly
//	[8B] inputAck:    uint64  (server-confirmed bytesReceived for this session)
//
//	If msg_type == screen:
//	  [8B] base          uint64  (absolute index of top screen row; changed[y] -> base+y)
//	  [2B] cursor_row    uint16
//	  [2B] cursor_col    uint16
//	  [2B] screen_height uint16  (full terminal height; rows below is sparse)
//	  [2B] num_changed   uint16
//	  [1B] cursor_style  uint8   (DECSCUSR style 0-6)
//	  [1B] cursor_flags  uint8   (bit0=hidden, bit1=bell, bit2=blink, bit3=altActive, bit4=scrollbackCleared)
//	  For each changed row:
//	    [2B] row_idx     uint16
//	    [row payload]
//
//	If msg_type == scroll:
//	  [8B] first_index  uint64  (absolute index of lines[0]; line i applies at first_index+i)
//	  [2B] num_lines    uint16
//	  For each line:
//	    [row payload]
//
//	If msg_type == resumeAck:
//	  inputAck above carries the value;
//	  [8B] serverEpoch  uint64 (process-start nanoseconds since epoch).
//	                    Client compares against last seen epoch to detect
//	                    server restart; on mismatch the resume protocol's
//	                    silent-data-loss case (server has no record of
//	                    bytes the client thinks are acked) is surfaced
//	                    instead of being papered over.
//	  [8B] committed    uint64 (absolute index of the next line to commit)
//	  [8B] oldestIndex  uint64 (absolute index of the oldest retained line)
//	  [1B] serverWireVersion uint8 (the server's wireProtocolVersion, so the
//	                    client can surface a stale-bundle skew; length-gated
//	                    optional tail, absent on pre-tier servers)
//	  [1B] ackFlags     uint8 (bit0 = ledgerLost: the resume key missed the
//	                    registry while the client claimed sentBytes > 0 — the
//	                    server cannot vouch for any previously sent input, so
//	                    the client must drop-and-notify instead of replaying;
//	                    same length-gated tail as serverWireVersion)
//
//	If msg_type == ackOnly:
//	  inputAck above carries the value; no body. Sent from the flush tick
//	  when input was applied but no content frame carried the advanced ack
//	  (input into a no-echo read), so acks never depend on output. Older
//	  clients ignore the unknown opcode (back-compatible, no version bump).
//
//	row payload:
//	  [2B] num_runs    uint16
//	  For each run:
//	    [2B] text_byte_len uint16
//	    [N B] text         utf-8 bytes
//	    [4B] fg            int32   (-1 = default fg)
//	    [4B] bg            int32   (-1 = default bg)
//	    [2B] attrs         uint16  (bit flags, see WireRun.A)
//	    [4B] uc            int32   (-1 = default underline color)
//	    [2B] url_len       uint16  (UTF-8 byte length of OSC 8 URL; 0 = no link)
//	    [N B] url          utf-8 bytes (OSC 8 hyperlink URI)
//
// Per-client ack patching: encodeScreenMsg / encodeScrollMsg accept a
// placeholder ack (typically 0) and return a template that flushLoop
// then clones and patches with the real per-client ack via
// withClientAck. This keeps the encode work O(frame_size) instead of
// O(clients × frame_size).

package terminal

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"

	"github.com/cplieger/web-terminal-engine/v4/vt"
)

const (
	wireMsgScreen    byte = 0
	wireMsgScroll    byte = 1
	wireMsgResumeAck byte = 2
	wireMsgModes     byte = 3
	wireMsgTitle     byte = 4
	wireMsgPong      byte = 5
	// wireMsgClipboard carries text an app copied via OSC 52, for the client to
	// write to the system clipboard. New opcodes are back-compatible (older
	// clients ignore unknown message types), so no wireProtocolVersion bump.
	wireMsgClipboard byte = 6
	// wireMsgAckOnly carries a bare inputAck for input that produced no output
	// frame within a flush tick (e.g. typing into `read -s`), so the client's
	// outbox trims promptly even with a silent app. Back-compatible new opcode
	// (older clients ignore unknown message types), so no version bump.
	wireMsgAckOnly byte = 7

	// WireProtocolVersion is the current binary-protocol revision. The client
	// sends it in the resume control message so the server can enforce its
	// declared compatibility floor and expose stale pairings. Bump it on any
	// breaking change to a frame layout or control-message shape. It mirrors
	// WIRE_PROTOCOL_VERSION in web/src/wire-compatibility.ts and is exported as
	// release metadata for Go consumers.
	//
	// v3: the modes frame gained a trailing kbdFlags byte (kitty keyboard
	// protocol). The change is decoder-tolerant both ways (an older client
	// ignores the extra byte; a newer client defaults kbdFlags to 0 for an
	// older server's shorter frame).
	//
	// v4: typed client→server framing. Text messages carry control JSON;
	// messages carry control JSON; binary messages carry PTY input with the
	// full byte alphabet. Negotiated in-band per connection: a binary
	// bootstrap resume declaring protocolVersion >= 4 ARMS the connection,
	// the resumeAck tail proves the server's revision to the client, and one
	// text `upgrade` control LATCHES typed mode. The v3 sentinel path
	// (0x00-prefixed binary controls) is accepted indefinitely pre-latch, so
	// the bump is the negotiation signal, not a compatibility break — v3
	// clients and version-silent clients are fully supported.
	WireProtocolVersion = 4

	// MinSupportedClientWireVersion is the oldest explicitly declared client
	// revision this server accepts. A version-silent client (0) remains
	// supported; a declared lower revision is refused with
	// WireIncompatibleCloseCode. Higher revisions warn but continue because a
	// future revision alone does not prove that its v4-compatible baseline is
	// unusable. Exported as directional release metadata for Go consumers.
	MinSupportedClientWireVersion = 3

	// Internal aliases keep the wire encoder and negotiation implementation
	// concise while the exported names above form the release metadata API.
	wireProtocolVersion           = WireProtocolVersion
	minSupportedClientWireVersion = MinSupportedClientWireVersion

	// typedFramingMinVersion is the first protocol revision with typed
	// client→server framing; a resume declaring at least this ARMS the
	// connection for the text-control latch.
	typedFramingMinVersion = 4

	// wireAckOffset is the byte offset of the inputAck field in
	// every server→client frame. Used by withClientAck to patch the
	// per-client ack into a pre-encoded template.
	wireAckOffset = 1
	wireAckSize   = 8

	// modeFlagBracketedPaste / modeFlagAppCursorKeys are the bit
	// positions in the modes message's flags byte. New flags MUST be
	// appended at higher bit positions to preserve back-compat with
	// older clients (unknown bits are ignored).
	modeFlagBracketedPaste byte = 1 << 0
	modeFlagAppCursorKeys  byte = 1 << 1
	modeFlagMouseSGR       byte = 1 << 2
	modeFlagFocusReporting byte = 1 << 3
	modeFlagAppKeypad      byte = 1 << 4
	modeFlagReverseVideo   byte = 1 << 5
	modeFlagMousePixels    byte = 1 << 6

	// resumeAckFlagLedgerLost is bit0 of the resumeAck ackFlags byte: set when
	// the resume key missed the registry while the client claimed sent bytes
	// (idle GC or cap eviction reclaimed the ledger). The client responds with
	// its designed loss semantic — drop the outbox and notify — instead of
	// guessing from an ambiguous received=0.
	resumeAckFlagLedgerLost byte = 1 << 0

	// resumeAckFlagHistoryPaging is bit1 of the resumeAck ackFlags byte: the
	// server DECLARES that it serves the `history` control and that its ring is
	// deep enough to back demand paging (see historyPagingDeclared). Capability
	// is declared rather than probed because the ack is the first frame of every
	// resume batch, so the client knows one RTT after attach with zero requests
	// spent and no way to mis-read a slow link as an old server. An unset bit
	// (or a server too old to carry the length-gated tail at all) reads as
	// unsupported: the client keeps its legacy resident-tail cap and never sends
	// a history control. See docs/paged-scrollback.md §4.5.
	resumeAckFlagHistoryPaging byte = 1 << 1
)

// WirePairIncompatibility reports whether a Go-server / TS-client PAIR is
// declared-incompatible before either half runs, and why. It is the
// peer-less, build-time form of the compatibility decision this package
// already makes twice at runtime — the server refusing a below-floor client
// (close code WireIncompatibleCloseCode) and the TS client refusing a
// below-floor server — so a consumer's release gate can reach the same
// verdict from two pairs of published integers instead of restating the rule.
//
// serverRev / serverMinClient are normally this package's WireProtocolVersion
// and MinSupportedClientWireVersion; they are parameters so a gate can also
// judge a pairing that does not involve the engine version it links against
// (a cross-version matrix, a proposed bump). clientRev / clientMinServer come
// from the TS half's WIRE_PROTOCOL_VERSION / MIN_SUPPORTED_SERVER_WIRE_VERSION.
//
// Returns "" when both declared floors admit the pairing; otherwise a
// human-readable reason naming the violated floor or the self-inconsistent
// half, the relevant revisions, and which half is behind. A caller decorates
// that with its own remediation (which pin to bump is the consumer's build-
// layout knowledge, not the engine's). Both CROSS-side floors are EXCLUSIVE
// bounds: a revision exactly at the peer's floor is compatible, matching the
// runtime checks. The WITHIN-side floor is inclusive: a half may declare a
// floor equal to its own revision (it just refuses every older peer).
//
// Deliberately stricter than runtime on version-silent input: a
// non-positive argument is a caller error (it cannot be distinguished from
// "not extracted"), so it is reported rather than tolerated. At runtime a
// version-silent peer declares 0 and stays supported; at build time a 0 means
// the gate failed to read the constant and must fail loudly.
//
// Higher-than-known revisions are compatible here, as at runtime: a future
// revision alone does not prove its compatible baseline is unusable.
//
// Each half is also checked for internal coherence: a half's minimum-supported-
// peer floor may not exceed its own revision, because such a build could not
// talk to a peer of its own revision. This is a BEHAVIOUR CHANGE relative to
// earlier releases (the check was added after v3.2.1): a pair whose halves are
// individually incoherent (e.g. clientRev 4 with clientMinServer 5) was
// previously judged on the cross-side floors alone and could be reported
// compatible; it is now reported incompatible. No correctly extracted pair of
// real released artifacts changes verdict — the engine's own constants have
// always satisfied the invariant (rev 4, floor 3 on both halves) — so a gate
// that starts failing here is reading garbage, which is exactly the outcome
// intended.
func WirePairIncompatibility(serverRev, serverMinClient, clientRev, clientMinServer int) string {
	// Case order is load-bearing. Positivity first (a missing constant is the
	// coarsest garbage), then each half's SELF-consistency, and only then the
	// cross-side floors. Self-inconsistency must precede the cross-side
	// verdicts because a garbage half otherwise yields a confident, misleading
	// skew diagnosis ("the Go half is behind, bump your pin") for input that
	// describes no real pairing at all — sending the caller to change a version
	// pin when the actual defect is in how it read the numbers.
	switch {
	case serverRev <= 0 || serverMinClient <= 0:
		return fmt.Sprintf(
			"server wire revisions must be positive (got rev %d, min client %d)",
			serverRev, serverMinClient)
	case clientRev <= 0 || clientMinServer <= 0:
		return fmt.Sprintf(
			"client wire revisions must be positive (got rev %d, min server %d)",
			clientRev, clientMinServer)
	case serverMinClient > serverRev:
		return fmt.Sprintf(
			"the Go server half is self-inconsistent: it demands client revision >= %d while itself speaking revision %d, so it could not talk to a client of its own build; these two numbers cannot both come from one released artifact, so treat this as corrupt or mis-extracted input (re-check how the constants were read) rather than a version skew to fix by bumping a pin",
			serverMinClient, serverRev)
	case clientMinServer > clientRev:
		return fmt.Sprintf(
			"the TS client half is self-inconsistent: it demands server revision >= %d while itself speaking revision %d, so it could not talk to a server of its own build; these two numbers cannot both come from one released artifact, so treat this as corrupt or mis-extracted input (re-check how the constants were read) rather than a version skew to fix by bumping a pin",
			clientMinServer, clientRev)
	case serverRev < clientMinServer:
		return fmt.Sprintf(
			"Go server wire revision %d is below the TS client's minimum supported server revision %d (the Go half is behind)",
			serverRev, clientMinServer)
	case clientRev < serverMinClient:
		return fmt.Sprintf(
			"TS client wire revision %d is below the Go server's minimum supported client revision %d (the TS half is behind)",
			clientRev, serverMinClient)
	default:
		return ""
	}
}

// encodeScreenMsg builds a binary screen frame containing only the
// rows whose indices appear in `changed`. screenHeight is the full
// terminal height (rowEls count on the client) — needed because rows
// is sparse on the wire.
//
// ack is non-zero only on the resume window frame (handleResume passes the
// resolved per-client ack); the per-flush dispatch path still encodes ack=0
// and patches the real value in via withClientAck.
func encodeScreenMsg(base uint64, screenHeight, curRow, curCol int, ack uint64, changed []int, rows [][]vt.WireRun, cursorStyle uint8, cursorHidden, cursorBlink, bell, altActive, scrollbackCleared bool) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, wireMsgScreen)
	buf = binary.LittleEndian.AppendUint64(buf, ack)
	buf = binary.LittleEndian.AppendUint64(buf, base)
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(curRow))
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(curCol))
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(screenHeight))
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(changed)))
	// Cursor metadata: style (0-6) and flags.
	buf = append(buf, cursorStyle)
	var cursorFlags byte
	if cursorHidden {
		cursorFlags |= 1
	}
	if bell {
		cursorFlags |= 2
	}
	if cursorBlink {
		cursorFlags |= 4
	}
	if altActive {
		cursorFlags |= 8
	}
	if scrollbackCleared {
		cursorFlags |= 16
	}
	buf = append(buf, cursorFlags)
	for _, idx := range changed {
		buf = binary.LittleEndian.AppendUint16(buf, clampU16(idx))
		if idx >= 0 && idx < len(rows) {
			buf = appendRowRuns(buf, rows[idx])
		} else {
			buf = binary.LittleEndian.AppendUint16(buf, 0) // num_runs = 0
		}
	}
	return buf
}

// encodeScrollMsg builds a binary scroll frame carrying committed
// history lines. firstIndex is the absolute index of lines[0]; the
// client applies each line at firstIndex+i into its absolute-indexed
// store (idempotent, so re-delivery never duplicates). Used both for
// live committed lines and for resume replay.
func encodeScrollMsg(ack, firstIndex uint64, lines [][]vt.WireRun) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, wireMsgScroll)
	buf = binary.LittleEndian.AppendUint64(buf, ack)
	buf = binary.LittleEndian.AppendUint64(buf, firstIndex)
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(lines)))
	for _, line := range lines {
		buf = appendRowRuns(buf, line)
	}
	return buf
}

// encodeResumeAck builds a resumeAck frame carrying the server's current
// per-session bytesReceived count, the server boot epoch, and the
// absolute-index bounds of retained history. committed is the absolute
// index of the next line to commit (one past the newest); oldestIndex
// is the absolute index of the oldest retained line. The client uses
// epoch to detect a server restart and (oldestIndex, committed) to
// detect a history-eviction gap on resume.
//
// The trailing [serverWireVersion, ackFlags] pair is the frame's third
// length-gated tail (>= 35 bytes): serverWireVersion lets the client surface
// a stale-bundle protocol skew ("reload required") instead of leaving the
// mismatch server-log-only, and ackFlags carries bit0 (ledgerLost — tells a
// resuming client its input ledger no longer exists so it must drop-and-notify
// rather than replay; see resumeAckFlagLedgerLost) and bit1 (historyPaging —
// declares demand-paged scrollback, see resumeAckFlagHistoryPaging). Older
// clients ignore the extra bytes, and one that reads the tail masks only the
// bits it knows; newer clients treat a shorter frame as "tail absent", which
// reads as no paging.
func encodeResumeAck(ack uint64, epochNanos int64, committed, oldestIndex uint64, ledgerLost, historyPaging bool) []byte {
	buf := make([]byte, 0, 35)
	buf = append(buf, wireMsgResumeAck)
	buf = binary.LittleEndian.AppendUint64(buf, ack)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(epochNanos)) // #nosec G115 -- epochNanos is always positive
	buf = binary.LittleEndian.AppendUint64(buf, committed)
	buf = binary.LittleEndian.AppendUint64(buf, oldestIndex)
	buf = append(buf, wireProtocolVersion)
	var flags byte
	if ledgerLost {
		flags |= resumeAckFlagLedgerLost
	}
	if historyPaging {
		flags |= resumeAckFlagHistoryPaging
	}
	buf = append(buf, flags)
	return buf
}

// encodeAckOnly builds a bare-ack frame (type + inputAck, no body). Emitted by
// the flush tick's ack sweep for clients whose session ledger advanced without
// any content frame carrying the new value — typically input into a no-echo
// read — so outbox trimming (and therefore loss-free resume accounting) never
// depends on the app producing output.
func encodeAckOnly(ack uint64) []byte {
	buf := make([]byte, 0, 9)
	buf = append(buf, wireMsgAckOnly)
	buf = binary.LittleEndian.AppendUint64(buf, ack)
	return buf
}

// encodeModesMsg builds a frame announcing the current DEC private
// mode state. The builder emits this whenever it observes a change in
// any tracked mode (see modesStable) so the client's input path can
// format paste, arrow keys, keypad, mouse, and reverse video correctly:
//
//	[1B] msg_type = 3 (modes)
//	[8B] inputAck (uint64)
//	[1B] flags
//	     bit 0: bracketed paste enabled (DEC ?2004h)
//	     bit 1: application cursor keys (DECCKM, CSI ?1h) enabled
//	     bit 2: SGR mouse encoding (DEC ?1006h) enabled
//	     bit 3: focus reporting (DEC ?1004h) enabled
//	     bit 4: application keypad (DECKPAM, ESC =) enabled
//	     bit 5: reverse video (DECSCNM, DEC ?5h) enabled
//	     bit 6: SGR-pixels mouse (DEC ?1016h) enabled
//	[2B] mouseMode (uint16): 0=off, 1000=normal, 1002=button-event, 1003=any-event
//	[1B] kbdFlags (uint8): kitty keyboard progressive-enhancement flags
//	     (bit0 disambiguate, bit1 event-types, bit2 alternate-keys; 0 = legacy).
//	     Added in wire protocol v3; a client decoding a pre-v3 frame defaults it
//	     to 0.
func encodeModesMsg(bracketedPaste, appCursorKeys, mouseSGR, focusReporting, appKeypad, reverseVideo, mousePixels bool, mouseMode uint16, kbdFlags uint8) []byte {
	buf := make([]byte, 0, 13)
	buf = append(buf, wireMsgModes)
	// inputAck placeholder (0); withClientAck patches the real per-client value at wireAckOffset.
	buf = binary.LittleEndian.AppendUint64(buf, 0)
	var flags byte
	if bracketedPaste {
		flags |= modeFlagBracketedPaste
	}
	if appCursorKeys {
		flags |= modeFlagAppCursorKeys
	}
	if mouseSGR {
		flags |= modeFlagMouseSGR
	}
	if focusReporting {
		flags |= modeFlagFocusReporting
	}
	if appKeypad {
		flags |= modeFlagAppKeypad
	}
	if reverseVideo {
		flags |= modeFlagReverseVideo
	}
	if mousePixels {
		flags |= modeFlagMousePixels
	}
	buf = append(buf, flags)
	buf = binary.LittleEndian.AppendUint16(buf, mouseMode)
	buf = append(buf, kbdFlags)
	return buf
}

// withClientAck returns a copy of template with the inputAck field
// patched to ack. Used by flushLoop to fan a single encoded frame out
// to multiple clients with their respective per-session ack values
// without re-encoding. The copy is mandatory: WebSocket libraries are
// allowed to retain or mutate (mask) the bytes through the duration
// of the write.
func withClientAck(template []byte, ack uint64) []byte {
	out := make([]byte, len(template))
	copy(out, template)
	if len(out) >= wireAckOffset+wireAckSize {
		binary.LittleEndian.PutUint64(out[wireAckOffset:], ack)
	}
	return out
}

// encodePongMsg builds a liveness pong frame: just the type tag and the
// fixed-width ack header every server→client frame carries (zero here —
// the pong carries no input-ack; its only purpose is to prove the socket
// is alive to the client's staleness probe). The client treats the mere
// arrival of any frame as liveness, so the body is intentionally empty.
//
//	[1B] msg_type = 5 (pong)
//	[8B] inputAck (uint64, always 0)
func encodePongMsg() []byte {
	buf := make([]byte, 0, 9)
	buf = append(buf, wireMsgPong)
	buf = binary.LittleEndian.AppendUint64(buf, 0)
	return buf
}

// Per-row wire overhead, split out so the ceiling arithmetic below and the
// tests that pin it read the same numbers as the encoder (see
// docs/paged-scrollback.md §4.2 — one helper drives stripping, page packing,
// and the tests so the three cannot drift).
const (
	// encodedRowCountSize is the row payload's leading num_runs field.
	encodedRowCountSize = 2
	// encodedRunFixedSize is the per-run fixed cost: 2 text_len + 4 fg +
	// 4 bg + 2 attrs + 4 uc + 2 url_len. Only text and url are variable.
	encodedRunFixedSize = 18
	// encodedScrollHeaderSize is encodeScrollMsg's header: 1 type + 8 ack +
	// 8 firstIndex + 2 numLines. A one-row reply is header + row payload, so
	// the largest row that can fit a budgeted reply is budget minus this.
	encodedScrollHeaderSize = 19
	// pageByteBudget is the hard per-reply bound for a history page. Browser
	// WebSockets expose no receive backpressure, so a delivered frame IS the
	// client's memory commitment.
	pageByteBudget = 256 * 1024
	// rowByteCeiling is the largest encoded row payload that still fits a
	// budgeted one-row reply (262125 at the shipped budget). A row above it is
	// re-encoded with hyperlink URIs stripped, which is arithmetically bounded
	// (~22 KB at the grid maximum) and therefore always fits.
	rowByteCeiling = pageByteBudget - encodedScrollHeaderSize
)

// encodedRowSize returns the exact number of bytes appendRowRuns will write
// for runs, including the leading num_runs field. It applies the same
// truncation the encoder applies, so the answer is the real wire size rather
// than an estimate.
func encodedRowSize(runs []vt.WireRun) int {
	size := encodedRowCountSize
	for _, run := range runs {
		size += encodedRunFixedSize
		size += len(truncateUTF8(run.T, 0xFFFF))
		size += len(truncateUTF8(run.U, 0xFFFF))
	}
	return size
}

// stripRowURIs returns a copy of runs with every hyperlink URI emptied and
// the autolink bit cleared, so a stripped run is indistinguishable from a
// never-linked one (rather than carrying AttrAutolink with an empty U, a state
// the wire format has never carried). Text and styling are untouched.
//
// The client re-linkifies stripped rows from VISIBLE text, so a server-stamped
// autolink whose URL was split by a style change or a soft wrap re-derives a
// PREFIX href, and an OSC 8 link whose text is not a URL loses its link
// entirely. That is the accepted degradation, reachable only above
// rowByteCeiling (see docs/paged-scrollback.md §4.2).
func stripRowURIs(runs []vt.WireRun) []vt.WireRun {
	out := make([]vt.WireRun, len(runs))
	copy(out, runs)
	for i := range out {
		out[i].U = ""
		out[i].A &^= vt.AttrAutolink
	}
	return out
}

// capRowRuns applies the per-row ceiling: a row whose encoding exceeds
// rowByteCeiling is re-encoded with its URIs stripped. The decision is a PURE
// FUNCTION of the canonical row — never of remaining frame budget or delivery
// path — so the same committed row encodes to identical bytes on every path
// (page, live flush, resume replay, screen) and repeated delivery of one index
// stays byte-identical, which is what makes the client's idempotence hold.
func capRowRuns(runs []vt.WireRun) []vt.WireRun {
	if encodedRowSize(runs) <= rowByteCeiling {
		return runs
	}
	return stripRowURIs(runs)
}

// appendRowRuns writes one row payload. The per-row ceiling is applied HERE,
// in the encoder every row-emitting path shares — page replies, live flush
// frames, resume replay chunks, and screen frames — deliberately including
// screen so a pathological row displays and pages identically (link-less in
// both) instead of showing links on screen and losing them the moment it
// scrolls off. The ceiling bounds every ROW; it does not bound the aggregate
// multi-row messages, which remain the pre-existing exposure named in
// docs/paged-scrollback.md §4.2.
func appendRowRuns(buf []byte, runs []vt.WireRun) []byte {
	runs = capRowRuns(runs)
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(runs)))
	for _, run := range runs {
		text := truncateUTF8(run.T, 0xFFFF)
		buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(text)))
		buf = append(buf, text...)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(run.F)) // #nosec G115 -- bit-cast
		buf = binary.LittleEndian.AppendUint32(buf, uint32(run.B)) // #nosec G115 -- bit-cast
		buf = binary.LittleEndian.AppendUint16(buf, run.A)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(run.Uc)) // #nosec G115 -- bit-cast
		url := truncateUTF8(run.U, 0xFFFF)
		buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(url)))
		buf = append(buf, url...)
	}
	return buf
}

func clampU16(n int) uint16 {
	if n < 0 {
		return 0
	}
	if n > 0xFFFF {
		return 0xFFFF
	}
	return uint16(n)
}

// truncateUTF8 returns s limited to at most maxBytes bytes without splitting a
// multi-byte rune, so the wire length field (clampU16(len)) always matches the
// appended payload. Only a pathological oversize run is clamped; every run
// <= maxBytes bytes (the normal case) is returned unchanged.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	t := s[:maxBytes]
	for t != "" && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t
}

// encodeTitleMsg builds a title frame carrying the window title string.
//
//	[1B] msg_type = 4 (title)
//	[8B] inputAck (uint64) — 0 placeholder; withClientAck patches the real value at send.
//	[2B] title_byte_len (uint16)
//	[NB] title (UTF-8 bytes)
func encodeTitleMsg(title string) []byte {
	title = truncateUTF8(title, 0xFFFF)
	buf := make([]byte, 0, 11+len(title))
	buf = append(buf, wireMsgTitle)
	// inputAck placeholder (0); withClientAck patches the real per-client value at wireAckOffset.
	buf = binary.LittleEndian.AppendUint64(buf, 0)
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(title)))
	buf = append(buf, title...)
	return buf
}

// encodeClipboardMsg builds a clipboard frame carrying text an app copied via
// OSC 52, for the client to write to the system clipboard:
//
//	[1B] msg_type = 6 (clipboard)
//	[8B] inputAck (uint64)
//	[2B] text_byte_len (uint16)
//	[N B] UTF-8 text
//
// The payload originates from an OSC sequence, whose buffer (maxOSCLen) caps it
// well below the uint16 length limit; the guard below is defensive only.
//
// The ack field is emitted as zero: this payload is encoded once per flush
// and fanned out to every client, and withClientAck stamps each client's
// real ack into the shared offset at write time (like every other payload
// dispatchFrame carries).
func encodeClipboardMsg(text []byte) []byte {
	body := text
	if len(body) > 0xFFFF {
		body = body[:0xFFFF]
	}
	buf := make([]byte, 0, 11+len(body))
	buf = append(buf, wireMsgClipboard)
	buf = binary.LittleEndian.AppendUint64(buf, 0)
	buf = binary.LittleEndian.AppendUint16(buf, clampU16(len(body)))
	buf = append(buf, body...)
	return buf
}
