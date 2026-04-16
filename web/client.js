/**
 * client.js — EasySync client state machine + WebSocket communication
 *
 * Implements the full client-side OT state machine as described in the
 * EasySync specification (Sections 10–11) and the AGENTS.md architecture notes.
 *
 * Client document state: A · X · Y
 *   A = latest server-acknowledged state (Changeset from empty doc to current server HEAD)
 *   X = submitted to server, awaiting ACK (identity if nothing pending)
 *   Y = local edits not yet submitted (grows as user types)
 *
 * Every 500ms the timer fires: if not waiting for ACK and Y is non-identity,
 * shift Y → X, send X to server, set waitingForAck = true.
 */

'use strict';

// ─────────────────────────────────────────────────────────────────
// Changeset operations (JS port of internal/ot/)
// ─────────────────────────────────────────────────────────────────

/**
 * identity(n) — returns the identity changeset for a document of length n.
 * Applying an identity changeset leaves the document unchanged.
 */
function identity(n) {
  if (n === 0) return { old_len: 0, new_len: 0, ops: [] };
  return { old_len: n, new_len: n, ops: [{ op: 'retain', n: n }] };
}

/**
 * isIdentity(cs) — true if the changeset makes no change.
 */
function isIdentity(cs) {
  if (cs.old_len !== cs.new_len) return false;
  if (cs.old_len === 0) return true;
  return cs.ops.length === 1 && cs.ops[0].op === 'retain' && cs.ops[0].n === cs.old_len;
}

/**
 * applyChangeset(text, cs) — applies ops to text and returns new string.
 * Mirrors the Go ApplyChangeset function.
 */
function applyChangeset(text, cs) {
  if (cs.old_len !== text.length) {
    throw new Error(`applyChangeset: old_len=${cs.old_len} != text.length=${text.length}`);
  }
  let result = '';
  let pos = 0;
  for (const op of cs.ops) {
    if (op.op === 'retain') {
      result += text.slice(pos, pos + op.n);
      pos += op.n;
    } else if (op.op === 'insert') {
      result += op.chars;
    } else if (op.op === 'delete') {
      pos += op.n;
    }
  }
  return result;
}

/**
 * compose(A, B) — returns the single changeset equivalent to applying A then B.
 * A.new_len must equal B.old_len.
 * Mirrors the Go Compose function.
 */
function compose(A, B) {
  const result = { old_len: A.old_len, new_len: B.new_len, ops: [] };

  // Helper: peek into a slice of ops with a position+offset cursor.
  function makeSlice(ops) {
    return { ops, pos: 0, off: 0 };
  }
  function peek(s) {
    if (s.pos >= s.ops.length) return null;
    const op = Object.assign({}, s.ops[s.pos]);
    if (op.op === 'insert') {
      op.chars = op.chars.slice(s.off);
    } else {
      op.n = op.n - s.off;
    }
    return op;
  }
  function consume(s, n) {
    while (n > 0 && s.pos < s.ops.length) {
      const op = s.ops[s.pos];
      const rem = op.op === 'insert' ? op.chars.length - s.off : op.n - s.off;
      if (n >= rem) { n -= rem; s.pos++; s.off = 0; }
      else { s.off += n; n = 0; }
    }
  }
  function addOp(op) {
    if (result.ops.length > 0) {
      const last = result.ops[result.ops.length - 1];
      if (last.op === op.op) {
        if (op.op === 'retain' || op.op === 'delete') { last.n += op.n; return; }
        if (op.op === 'insert') { last.chars += op.chars; return; }
      }
    }
    result.ops.push(Object.assign({}, op));
  }

  const aSlice = makeSlice(A.ops);
  const bSlice = makeSlice(B.ops);

  while (true) {
    const bOp = peek(bSlice);
    const aOp = peek(aSlice);
    if (!bOp && !aOp) break;

    // B inserts: emit, consume nothing from A.
    if (bOp && bOp.op === 'insert') {
      addOp({ op: 'insert', chars: bOp.chars });
      consume(bSlice, bOp.chars.length);
      continue;
    }
    // A deletes: emit delete, consume nothing from B.
    if (aOp && aOp.op === 'delete') {
      addOp({ op: 'delete', n: aOp.n });
      consume(aSlice, aOp.n);
      continue;
    }
    if (!aOp || !bOp) break;

    const aLen = aOp.op === 'insert' ? aOp.chars.length : aOp.n;
    const bLen = bOp.n;
    const n = Math.min(aLen, bLen);

    if (aOp.op === 'retain' && bOp.op === 'retain') addOp({ op: 'retain', n });
    else if (aOp.op === 'retain' && bOp.op === 'delete') addOp({ op: 'delete', n });
    else if (aOp.op === 'insert' && bOp.op === 'retain') addOp({ op: 'insert', chars: aOp.chars.slice(0, n) });
    else if (aOp.op === 'insert' && bOp.op === 'delete') { /* B deletes A's insert — net nothing */ }

    consume(aSlice, n);
    consume(bSlice, n);
  }
  return result;
}

/**
 * follow(A, B) → B'
 *
 * Computes B rebased on top of A. After applying A to the base doc, applying
 * B' gives the same merged result as applying both A and B to the base doc.
 *
 * This is the core OT operation. Mirrors the Go Follow function exactly,
 * including the lexicographic tie-breaker for simultaneous inserts.
 */
function follow(A, B) {
  const result = { old_len: A.new_len, new_len: 0, ops: [] };

  function makeSlice(ops) { return { ops, pos: 0, off: 0 }; }
  function peek(s) {
    if (s.pos >= s.ops.length) return null;
    const op = Object.assign({}, s.ops[s.pos]);
    if (op.op === 'insert') op.chars = op.chars.slice(s.off);
    else op.n = op.n - s.off;
    return op;
  }
  function consume(s, n) {
    while (n > 0 && s.pos < s.ops.length) {
      const op = s.ops[s.pos];
      const rem = op.op === 'insert' ? op.chars.length - s.off : op.n - s.off;
      if (n >= rem) { n -= rem; s.pos++; s.off = 0; }
      else { s.off += n; n = 0; }
    }
  }
  function addOp(op) {
    if (result.ops.length > 0) {
      const last = result.ops[result.ops.length - 1];
      if (last.op === op.op) {
        if (op.op === 'retain') { last.n += op.n; result.new_len += op.n; return; }
        if (op.op === 'delete') { last.n += op.n; return; }
        if (op.op === 'insert') { last.chars += op.chars; result.new_len += op.chars.length; return; }
      }
    }
    result.ops.push(Object.assign({}, op));
    if (op.op === 'retain') result.new_len += op.n;
    if (op.op === 'insert') result.new_len += op.chars.length;
  }

  const aSlice = makeSlice(A.ops);
  const bSlice = makeSlice(B.ops);

  while (true) {
    const aOp = peek(aSlice);
    const bOp = peek(bSlice);
    if (!aOp && !bOp) break;

    // Both inserting at same position: tie-breaker by lexicographic comparison.
    if (aOp && aOp.op === 'insert' && bOp && bOp.op === 'insert') {
      if (aOp.chars <= bOp.chars) {
        // A goes first → B' must retain over A's inserted chars.
        addOp({ op: 'retain', n: aOp.chars.length });
        consume(aSlice, aOp.chars.length);
      } else {
        // B goes first → B' inserts B's text now.
        addOp({ op: 'insert', chars: bOp.chars });
        consume(bSlice, bOp.chars.length);
      }
      continue;
    }

    // A inserts: B must retain over A's new chars.
    if (aOp && aOp.op === 'insert') {
      addOp({ op: 'retain', n: aOp.chars.length });
      consume(aSlice, aOp.chars.length);
      continue;
    }

    // B inserts: B's intent preserved.
    if (bOp && bOp.op === 'insert') {
      addOp({ op: 'insert', chars: bOp.chars });
      consume(bSlice, bOp.chars.length);
      continue;
    }

    if (!aOp || !bOp) break;

    const n = Math.min(aOp.n, bOp.n);

    if (aOp.op === 'retain' && bOp.op === 'retain') addOp({ op: 'retain', n });
    else if (aOp.op === 'retain' && bOp.op === 'delete') addOp({ op: 'delete', n });
    else if (aOp.op === 'delete' && bOp.op === 'retain') { /* A deleted; B's retain is dropped */ }
    else if (aOp.op === 'delete' && bOp.op === 'delete') { /* both deleted — nothing */ }

    consume(aSlice, n);
    consume(bSlice, n);
  }
  return result;
}

/**
 * diffToChangeset(oldText, newText) — builds a changeset from old to new text.
 * Uses a simple O(n*m) LCS diff. Sufficient for UI use.
 */
function diffToChangeset(oldText, newText) {
  const n = oldText.length, m = newText.length;
  // LCS DP table
  const dp = Array.from({ length: n + 1 }, () => new Int32Array(m + 1));
  for (let i = 1; i <= n; i++)
    for (let j = 1; j <= m; j++)
      dp[i][j] = oldText[i - 1] === newText[j - 1]
        ? dp[i - 1][j - 1] + 1
        : Math.max(dp[i - 1][j], dp[i][j - 1]);

  // Backtrack
  const segs = [];
  let i = n, j = m;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && oldText[i - 1] === newText[j - 1]) {
      segs.push({ kind: '=', c: oldText[i - 1] }); i--; j--;
    } else if (j > 0 && (i === 0 || dp[i][j - 1] >= dp[i - 1][j])) {
      segs.push({ kind: '+', c: newText[j - 1] }); j--;
    } else {
      segs.push({ kind: '-', c: oldText[i - 1] }); i--;
    }
  }
  segs.reverse();

  // Build changeset ops (merged).
  const ops = [];
  function addOp(op) {
    if (ops.length > 0) {
      const last = ops[ops.length - 1];
      if (last.op === op.op) {
        if (op.op === 'retain' || op.op === 'delete') { last.n += op.n; return; }
        if (op.op === 'insert') { last.chars += op.chars; return; }
      }
    }
    ops.push(Object.assign({}, op));
  }
  for (const s of segs) {
    if (s.kind === '=') addOp({ op: 'retain', n: 1 });
    else if (s.kind === '+') addOp({ op: 'insert', chars: s.c });
    else addOp({ op: 'delete', n: 1 });
  }
  return { old_len: n, new_len: m, ops };
}

// ─────────────────────────────────────────────────────────────────
// Client State Machine
// ─────────────────────────────────────────────────────────────────

let state = {
  clientId: localStorage.getItem('collab-editor-client-id') || null,
  A: null,               // Changeset: last server-acknowledged state
  X: null,               // Changeset: submitted, awaiting ACK
  Y: null,               // Changeset: local, unsubmitted
  serverRev: 0,          // The revision A is based on
  nextSubmissionId: parseInt(localStorage.getItem('collab-editor-submission-id') || '0', 10),
  waitingForAck: false,  // True while X is in-flight

  // The text the server last confirmed (for diffing).
  // This is Apply(Apply("", A), X) at submit time, but we track it separately.
  baseText: '',          // text as of last ACK or initial connect
  editorText: '',        // current textarea value (may include Y)
};

let ws = null;             // WebSocket connection
let submitTimer = null;    // 500ms submission interval

// ─────────────────────────────────────────────────────────────────
// Connection management
// ─────────────────────────────────────────────────────────────────


function connectToServer() {
  const addr = document.getElementById('server-input').value.trim();
  if (!addr) return;

  if (!state.clientId) {
    state.clientId = 'client-' + Math.random().toString(36).slice(2, 10) + '-' + Date.now();
    localStorage.setItem('collab-editor-client-id', state.clientId);
    state.nextSubmissionId = 0;
    localStorage.setItem('collab-editor-submission-id', '0');
  }

  document.getElementById('client-id-display').textContent = state.clientId.slice(0, 16) + '…';
  document.getElementById('server-addr').textContent = addr;

  log('info', `Connecting to ${addr} as ${state.clientId}`);

  ws = new WebSocket(addr);
  ws.onopen = onWsOpen;
  ws.onmessage = onWsMessage;
  ws.onclose = onWsClose;
  ws.onerror = (e) => log('error', 'WebSocket error: ' + e.message);
}

function disconnectFromServer() {
  if (ws) ws.close();
}

function onWsOpen() {
  setConnected(true);
  log('info', 'WebSocket connected. Sending CONNECT handshake.');

  // Send CONNECT, including our last known rev for catch-up.
  sendMsg('CONNECT', {
    client_id: state.clientId,
    last_known_rev: state.serverRev,
  });
}

function onWsClose() {
  setConnected(false);
  clearInterval(submitTimer);
  submitTimer = null;
  ws = null;
  log('warn', 'WebSocket disconnected.');
  document.getElementById('editor').disabled = true;
}

function onWsMessage(evt) {
  let env;
  try { env = JSON.parse(evt.data); }
  catch (e) { log('error', 'Invalid JSON from server'); return; }

  const { type, payload } = env;

  switch (type) {
    case 'CONNECT_ACK': handleConnectAck(payload); break;
    case 'ACK': handleAck(payload); break;
    case 'BROADCAST': handleBroadcast(payload); break;
    case 'REDIRECT': handleRedirect(payload); break;
    case 'ERROR': log('error', `Server error ${payload.code}: ${payload.message}`); break;
    default: log('warn', `Unknown message type: ${type}`);
  }
}

// ─────────────────────────────────────────────────────────────────
// Message handlers
// ─────────────────────────────────────────────────────────────────

/**
 * handleConnectAck: server sends us HEAD state + catch-up revisions.
 * We reset A to the server's current HEAD text and restart the submit loop.
 */
function handleConnectAck(payload) {
  const { head_text, head_rev, catch_up } = payload;
  log('info', `CONNECT_ACK: head_rev=${head_rev}, catch_up=${catch_up ? catch_up.length : 0} revisions`);

  const n = head_text.length;

  if (state.serverRev === 0) {
    // Case 1: First connection. The server's head_text is the source of truth.
    state.baseText = head_text;
    state.editorText = head_text;
    state.serverRev = head_rev;
  } else {
    // Case 2: Reconnect. We apply catch-up revisions to our EXISTING baseText.
    // (Note: In this simple client, we still reset A/X/Y for robustness,
    // but applying catch-up to baseText keeps the rev numbers in sync).
    if (catch_up && catch_up.length > 0) {
      for (const rec of catch_up) {
        try {
          state.baseText = applyChangeset(state.baseText, rec.changeset);
          // Rebase X and Y against this historical change we missed.
          state.X = follow(rec.changeset, state.X);
          state.Y = follow(follow(state.X, rec.changeset), state.Y);
        } catch (e) {
          log('error', 'catch-up apply failed: ' + e.message);
        }
      }
    }
    state.serverRev = head_rev;
  }

  // Common initialization after state is caught up.
  // Note: We NO LONGER reset A/X/Y to identity here.
  // We re-derive A from the new baseText.
  const nt = state.baseText.length;
  state.A = { old_len: 0, new_len: nt, ops: nt === 0 ? [] : [{ op: 'insert', chars: state.baseText }] };

  // Update the editor text to match the merged result of A · X · Y.
  try {
    const afterAX = applyChangeset(state.baseText, state.X);
    state.editorText = applyChangeset(afterAX, state.Y);
  } catch (e) {
    state.editorText = state.baseText;
    state.X = identity(nt);
    state.Y = identity(nt);
  }

  // Show the document in the editor.
  const editor = document.getElementById('editor');
  editor.value = state.editorText;
  editor.disabled = false;
  updateStatus();

  // Start the 500ms submission loop.
  if (submitTimer) clearInterval(submitTimer);
  submitTimer = setInterval(submitTick, 500);
}

/**
 * handleAck: our X was committed. Fold X into A.
 * Section 11.3: A ← A·X, X ← I
 */
function handleAck(payload) {
  const { new_rev } = payload;
  log('ot', `ACK: new_rev=${new_rev}`);

  // A ← compose(A, X)
  try {
    state.A = compose(state.A, state.X);
  } catch (e) {
    log('error', 'compose(A, X) failed on ACK: ' + e.message);
  }

  // X ← identity
  state.X = identity(state.A.new_len);
  state.serverRev = new_rev;
  state.waitingForAck = false;

  // Update the base text to reflect what the server has now confirmed.
  try {
    state.baseText = applyChangeset('', state.A);
  } catch (e) {
    state.baseText = document.getElementById('editor').value;
  }

  updateStatus();
}

/**
 * handleBroadcast: another client's changeset B was committed.
 * Section 11.4: update A, X, Y and apply visual diff D to the editor.
 *
 *   A' = compose(A, B)
 *   X' = follow(B, X)
 *   Y' = follow(follow(X, B), Y)
 *   D  = follow(Y, follow(X, B))
 */
function handleBroadcast(payload) {
  const { changeset: B, new_rev } = payload;
  log('ot', `BROADCAST: new_rev=${new_rev}, ops=${B.ops.length}`);

  try {
    const editor = document.getElementById('editor');
    const currentText = editor.value;

    // ── Pre-synchronization: Consolidate local edits ──
    // Before we can rebase against B, we must ensure Y covers all typing
    // that happened since the last submitTick or handleBroadcast.
    const afterAX = applyChangeset(state.baseText, state.X);
    state.Y = diffToChangeset(afterAX, currentText);

    // ── OTrebasing ──
    const XfollowB = follow(state.X, B);      // f(X, B)
    const BfollowX = follow(B, state.X);      // f(B, X) — used to compute X'

    const A_prime = compose(state.A, B);       // A' = A·B
    const X_prime = BfollowX;                 // X' = f(B, X)
    const Y_prime = follow(XfollowB, state.Y); // Y' = f(f(X,B), Y)
    const D = follow(state.Y, XfollowB);       // D = f(Y, f(X, B)) — apply to screen

    // ── Apply D to the current document view ──
    const cursorStart = editor.selectionStart;
    const cursorEnd = editor.selectionEnd;

    const newText = applyChangeset(currentText, D);
    editor.value = newText;

    // Restore cursor, adjusted for the remote insertion/deletion.
    const newCursorStart = adjustCursor(cursorStart, D);
    const newCursorEnd = adjustCursor(cursorEnd, D);
    editor.selectionStart = newCursorStart;
    editor.selectionEnd = newCursorEnd;

    state.A = A_prime;
    state.X = X_prime;
    state.Y = Y_prime;
    state.serverRev = new_rev;

    // Update base text.
    try {
      state.baseText = applyChangeset('', state.A);
    } catch (_) {
      state.baseText = applyChangeset(state.baseText, B);
    }

    updateStatus();
  } catch (e) {
    log('error', 'broadcast handling failed: ' + e.message);
    console.error(e);
  }
}

/**
 * handleRedirect: we connected to a follower. The server tells us to go to the leader.
 * We disconnect and reconnect to the leader's WebSocket address.
 */
function handleRedirect(payload) {
  const { leader_addr } = payload;
  log('warn', `REDIRECT to leader: ${leader_addr}`);

  // Build a WebSocket URL from the gRPC address.
  // Convention: gRPC port n → WS port n-4000. e.g. :12000 → :8000.
  // The leader sends its WS address directly in this implementation.
  if (ws) ws.close();

  // Try to derive a WS address from the leader gRPC addr.
  // If leader_addr is a gRPC addr like "localhost:12001", WS is "ws://localhost:8081".
  const wsAddr = grpcToWsAddr(leader_addr);
  document.getElementById('server-input').value = wsAddr;
  setTimeout(() => connectToServer(), 500);
}

/**
 * grpcToWsAddr: convert "host:grpcPort" to "ws://host:wsPort".
 * Convention in our cluster: WS port = gRPC port - 4000 (12000→8080, 12001→8081, etc.)
 */
function grpcToWsAddr(grpcAddr) {
  if (!grpcAddr) return document.getElementById('server-input').value;
  const parts = grpcAddr.split(':');
  if (parts.length < 2) return 'ws://' + grpcAddr + '/ws';
  const host = parts[0] || 'localhost';
  const grpcPort = parseInt(parts[parts.length - 1], 10);
  // Match the user's local port convention:
  // 12000 (gRPC) -> 8080 (WS)
  // 12001 (gRPC) -> 8081 (WS)
  // Offset = 3920
  const wsPort = grpcPort - 3920;
  return `ws://${host}:${wsPort}/ws`;
}

// ─────────────────────────────────────────────────────────────────
// Submit loop (500ms timer)
// ─────────────────────────────────────────────────────────────────

/**
 * submitTick — called every 500ms.
 * If not waiting for ACK and Y has changes, move Y → X and send SUBMIT.
 *
 * We compute Y by diffing the current editor text against the base text
 * (lazy diffing approach — handles paste, select-delete, etc. automatically).
 */
function submitTick() {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  // If we already have a pending X, we are either waiting for its ACK
  // or we just reconnected and need to re-submit it.
  if (state.waitingForAck) {
    // Only re-submit if the WebSocket is open.
    if (!lastSubmissionTime || Date.now() - lastSubmissionTime > 2000) {
      log('info', `Re-submitting X: submission_id=${state.nextSubmissionId}`);
      sendSubmit();
    }
    return;
  }

  // Compute the current Y by diffing editor text vs base text.
  const editor = document.getElementById('editor');
  const currentText = editor.value;

  // Base text = text after A and X have been applied.
  // Since X is identity when we're free to submit, base = A applied to "".
  let afterAX = state.baseText;
  if (!isIdentity(state.X)) {
    try { afterAX = applyChangeset(afterAX, state.X); } catch (_) { }
  }

  const Y = diffToChangeset(afterAX, currentText);
  if (isIdentity(Y)) return; // nothing to submit

  // Move Y → X.
  state.X = Y;
  state.Y = identity(currentText.length);
  state.waitingForAck = true;
  state.nextSubmissionId++;
  localStorage.setItem('collab-editor-submission-id', state.nextSubmissionId.toString());

  sendSubmit();
}

let lastSubmissionTime = 0;
function sendSubmit() {
  lastSubmissionTime = Date.now();
  log('ot', `SUBMIT: submission_id=${state.nextSubmissionId}, base_rev=${state.serverRev}, ops=${state.X.ops.length}`);

  sendMsg('SUBMIT', {
    client_id: state.clientId,
    submission_id: state.nextSubmissionId,
    base_rev: state.serverRev,
    changeset: state.X,
  });

  updateStatus();
}

// ─────────────────────────────────────────────────────────────────
// Cursor adjustment
// ─────────────────────────────────────────────────────────────────

/**
 * adjustCursor(pos, cs) — shift a cursor position through a changeset.
 * If the changeset inserts chars before `pos`, shift pos forward.
 * If it deletes chars before `pos`, shift pos back (clamped to 0).
 */
function adjustCursor(pos, cs) {
  let srcPos = 0;
  let newPos = pos;
  for (const op of cs.ops) {
    if (op.op === 'retain') {
      srcPos += op.n;
    } else if (op.op === 'insert') {
      if (srcPos <= pos) newPos += op.chars.length;
    } else if (op.op === 'delete') {
      const overlap = Math.min(op.n, Math.max(0, pos - srcPos));
      newPos -= overlap;
      srcPos += op.n;
    }
    if (srcPos > pos) break;
  }
  return Math.max(0, newPos);
}

// ─────────────────────────────────────────────────────────────────
// Utility
// ─────────────────────────────────────────────────────────────────

function sendMsg(type, payload) {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  ws.send(JSON.stringify({ type, payload }));
}

function setConnected(connected) {
  const el = document.getElementById('conn-status');
  el.textContent = connected ? 'Connected' : 'Disconnected';
  el.className = connected ? 'connected' : '';
  document.getElementById('connect-btn').disabled = connected;
  document.getElementById('disconnect-btn').disabled = !connected;
}

function updateStatus() {
  document.getElementById('rev-display').textContent = state.serverRev;
  document.getElementById('pending-display').textContent =
    state.waitingForAck ? 'waiting ACK' : (isIdentity(state.X) ? 'none' : 'X pending');
}

let logCount = 0;
function log(level, msg) {
  const container = document.getElementById('log-container');
  const el = document.createElement('div');
  el.className = 'log-entry ' + level;
  el.textContent = `[${new Date().toISOString().slice(11, 23)}] ${msg}`;
  container.appendChild(el);
  // Keep log manageable.
  if (++logCount > 200) {
    container.removeChild(container.firstChild);
    logCount--;
  }
  container.scrollTop = container.scrollHeight;
}

// ─────────────────────────────────────────────────────────────────
// Editor event — track keystrokes (for future Y accumulation).
// For now the 500ms timer handles diffing automatically.
// ─────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────
// Stress Test / Automation (Requested by USER)
// ─────────────────────────────────────────────────────────────────

let stressTimer = null;
let stressRand = null;

function toggleStressTest() {
  if (stressTimer) {
    stopStressTest();
  } else {
    startStressTest();
  }
}

function startStressTest() {
  const cps = parseInt(document.getElementById('stress-cps').value, 10);
  const seed = parseInt(document.getElementById('stress-seed').value, 10);
  const duration = parseInt(document.getElementById('stress-duration').value, 10);
  const editor = document.getElementById('editor');

  if (!editor || editor.disabled) {
    log('error', 'Cannot start stress test: Editor is disabled');
    return;
  }

  log('warn', `Starting stress test: CPS=${cps}, Seed=${seed}, Duration=${duration}s`);
  document.getElementById('stress-start-btn').textContent = 'Stop Stress Test';
  document.getElementById('stress-start-btn').style.background = '#f44747';

  stressRand = mulberry32(seed);
  const intervalMs = 1000 / cps;
  const startedAt = Date.now();

  stressTimer = setInterval(() => {
    if (Date.now() - startedAt >= duration * 1000) {
      stopStressTest();
      return;
    }
    performRandomOp();
  }, intervalMs);
}

function stopStressTest() {
  if (stressTimer) {
    clearInterval(stressTimer);
    stressTimer = null;
  }
  document.getElementById('stress-start-btn').textContent = 'Start Stress Test';
  document.getElementById('stress-start-btn').style.background = '#0e639c';
  log('info', 'Stress test stopped.');
}

function mulberry32(a) {
  return function () {
    let t = a += 0x6D2B79F5;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function performRandomOp() {
  const editor = document.getElementById('editor');
  const weights = {
    insert: parseInt(document.getElementById('weight-insert').value, 10) / 100,
    backspace: parseInt(document.getElementById('weight-backspace').value, 10) / 100,
    enter: parseInt(document.getElementById('weight-enter').value, 10) / 100,
    move: parseInt(document.getElementById('weight-move').value, 10) / 100,
  };

  const x = stressRand();
  let action = 'insert';
  let sum = 0;
  for (const [k, p] of Object.entries(weights)) {
    sum += p;
    if (x < sum) { action = k; break; }
  }

  const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ';
  const pos = editor.selectionStart;

  switch (action) {
    case 'insert':
      const s = chars[Math.floor(stressRand() * chars.length)];
      editor.setRangeText(s, pos, pos, 'end');
      break;
    case 'backspace':
      if (pos > 0) editor.setRangeText('', pos - 1, pos, 'end');
      break;
    case 'enter':
      editor.setRangeText('\n', pos, pos, 'end');
      break;
    case 'move':
      const dir = stressRand() < 0.5 ? -1 : 1;
      const newPos = Math.max(0, Math.min(editor.value.length, pos + dir));
      editor.setSelectionRange(newPos, newPos);
      break;
  }
  // Trigger input event for diffing.
  editor.dispatchEvent(new Event('input', { bubbles: true }));
}
