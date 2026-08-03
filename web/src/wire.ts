// Pure wire-format helpers for the client → server WebSocket protocol.
//
// v3 framing (the bootstrap/fallback encoding): every client→server message
// is a binary frame whose first byte tags the message type:
//
//   0x00  control message (JSON-encoded {type, ...} payload)
//   any   raw terminal input bytes
//
// Disambiguation rule (amended with the parse-fallback): a solitary [0x00],
// and any 0x00-leading frame that is not a valid control message, are literal
// terminal input; 0x00 + valid control JSON is the reserved control channel.
// The client additionally never emits a multi-byte 0x00-leading input frame
// in v3 mode (sendInputFrames splits leading NULs into solitary frames), so
// in practice the reserved channel only ever carries real controls.
//
// v4 framing (see WIRE_PROTOCOL_VERSION in wire-compatibility.ts) retires the sentinel once a
// socket upgrades: control messages travel as WebSocket TEXT frames and
// binary frames are full-alphabet PTY input. This module's controlFrame stays
// the encoding for the v3 bootstrap (every socket's first resume) and for
// v3-mode sockets (older servers).
//
// Pulled into a dedicated module so unit tests can exercise the framing
// without spinning up a WebSocket. Pure: no DOM, no WebSocket, no
// module-level state.

/** Type tag prefix for control messages. */
export const CONTROL_FRAME_PREFIX = 0x00;

/**
 * controlFrame builds the 0x00-prefixed binary frame containing the
 * JSON encoding of msg. The result is the exact bytes to pass to
 * `WebSocket.send(...)`.
 *
 * Returns a Uint8Array backed by a fresh ArrayBuffer (not a
 * SharedArrayBuffer slice) so it satisfies WebSocket.send's
 * BufferSource parameter.
 */
export function controlFrame(msg: unknown): Uint8Array<ArrayBuffer> {
  const body = new TextEncoder().encode(JSON.stringify(msg));
  const frame = new Uint8Array(new ArrayBuffer(body.length + 1));
  frame[0] = CONTROL_FRAME_PREFIX;
  frame.set(body, 1);
  return frame;
}
