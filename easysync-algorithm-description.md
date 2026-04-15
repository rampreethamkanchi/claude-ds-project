# this is the description of the basic version of easysync algorithm etherpad used many years ago, we might modify it for our project
## Etherpad and EasySync Technical Manual
## 1 Documents Contents

- A document is a list of characters, or a string.
- A document can also be represented as a list ofchangesets.

## 2 Changesets

- A changeset represents a change to a document.
- A changeset can be applied to a document to produce a new document.
- When a document is represented as a list of changesets, it is assumed that
    the first changeset applies to the empty document, [].

## 3 Changeset representation

```
(n →n′)[c 1 , c 2 , c 3 , ...]
where
```
```
n is the length of the document before the change,
```
```
n′is the length of the document after the change,
```
```
[c 1 , c 2 , c 3 , ...] is an array of n′ characters that described the document after
the change.
```
```
Note that∀ci: 0≤i≤n′is either an integer or a character.
```
- Integers represent retained characters in the original document.
- Characters represent insertions.

## 4 Constraints on Changesets

- Changesets are canonical and therefor comparable. When represented in
    computer memory, we always use the same representation for the same
    changeset. If the memory representation of two changesets differ, they
    must be different changesets.
- Changesets are compact. Thus, if there are two ways to represent a change-
    set in computer memory, then we always use the representation that takes
    up the fewest bytes.

Later we will discuss optimizations to changeset representation (using “strips”
and other such techniques). The two constraints must apply to any representa-
tion of changesets.


## 5 Notation

- We use the algebraic multiplication notation to represent changeset appli-
    cation.
- While changesets are defined as operations on documents, documents
    themselves are represented as a list of changesets, initially applying to
    the empty document.

Example A= (0→5)[“hello”]B= (5→11)[0− 4 ,“world”]
We can write the document “hello world” as A·B or justAB. Note
that the “initial document” can be made into the changeset (0→ N)[“<
the document text>”].
When A and B are changesets, we can also refer to (AB) as “the composi-
tion” of A and B. Changesets are closed under composition.

## 6 Composition of Changesets

For any two changesets A,B such that

```
A= (n 1 →n 2 )[···]
```
```
B= (n 2 →n 3 )[···]
```
it is clear that there is a third changeset C= (n 1 →n 3 )[···] such that applying
C to a document X yields the same resulting document as does applying A and
then B. In this case, we write AB=C.
Given the representation from Section 3, it is straightforward to compute
the composition of two changesets.

## 7 Changeset Merging

Now we come to realtime document editing. Suppose two different users make
two different changes to the same document at the same time. It is impossible
to compose these changes. For example, if we have the document X of length
n, we may have A= (n→na)[... na characters],B= (n→nb)[... nb characters]
wheren 6 =na 6 =nb.
It is impossible to compute (XA)B because B can only be applied to a
document of length n, and (XA) has length na. Similarly, A cannot be applied
to (XB) because (XB) has length nb.
This is where merging comes in. Merging takes two changesets that apply
to the same initial document (and that cannot be composed), and computes a
single new changeset that preserves the intent of both changes. The merge of
A and B is written as m(A, B). For the Etherpad system to work, we require
that m(A, B) = m(B, A).


Aside from what we have said so far about merging, there are many different
implementations that will lead to a workable system. We have created one
implementation for text that has the following constraints.

## 8 Follows

When users A and B have the same document X on their screen, and they
proceed to make respective changesets A and B, it is no use to compute m(A, B),
because m(A, B) applies to document X, but the users are already looking at
document XA and XB. What we really want is to compute B′ and A′ such
that
XAB′=XBA′=Xm(A, B)
“Following” computes these B′ and A′ changesets. The definition of the
“follow” function f is such that Af(A, B) = Bf(B, A) = m(A, B) = m(B, A).
When we compute f(A, B)

- Insertions in A become retained characters in f(A, B)
- Insertions in B become insertions in f(A, B)
- Retain whatever characters are retained in both A and B

Example Suppose we have the initial document X= (0→8)[“baseball”] and
user A changes it to “basil” with changeset A, and user B changes it to “below”
with changeset B.
We have X= (0→8)[“baseball”]
A= (8→5)[0− 1 ,“si”,7]
B= (8→5)[0,“e”, 6 ,“ow”]

```
First we compute the merge m(A, B) = m(B, A) according to the constraints
```
```
m(A, B) = (8→6)[0,”e”,”si”,”ow”] = (8→6)[0,“esiow”]
Then we need to compute the followsB′=f(A, B) andA′=f(B, A).
```
B′=f(A, B) = (5→6)[0,“e”, 2 , 3 ,“ow”]
Note that the numbers 0, 2, and 3 are indices intoA= (8→5)[0, 1 ,“si”,7]
0 1 2 3 4
0 1 s i 7
A′=f(B, A) = (5→6)[0, 1 ,”si”, 3 ,4]
We can now double check thatAB′=BA′=m(A, B) = (8→6)[0,“esiow”].
Now that we have made the mathematical meaning of the preceding pages
complete, we can build a client/server system to support realtime editing by
multiple users.


## 9 System Overview

There is a server that holds the current state of a document. Clients (users) can
connect to the server from their web browsers. The clients and server maintain
state and can send messages to one another in real-time, but because we are in
a web browser scenario, clients cannot send each other messages directly, and
must go through the server always. (This may distinguish from prior art?)
The other critical design feature of the system is thatA client must always
be able to edit their local copy of the document, so the user is never blocked from
typing because of waiting to send or receive data.

## 10 Client State

At any moment in time, a client maintains its state in the form of 3 changesets.
The client document looks like A · X · Y, where
A is the latest server version, the composition of all changesets committed
to the server, from this client or from others, that the server has informed this
client about. Initially A = (0→N)[<initial document text>].
X is the composition of all changesets this client has submitted to the server
but has not heard back about yet. Initially X = (N→N)[0, 1 , 2 ,... , N−1], in
other words, the identity, henceforth denoted IN.
Y is the composition of all changesets this client has made but has not yet
submitted to the server yet. Initially Y = (N→N)[0, 1 , 2 ,... , N−1].

## 11 Client Operations

A client can do 5 things.

1. Incorporate new typing into local state
2. Submit a changeset to the server
3. Hear back acknowledgement of a submitted changeset
4. Hear from the server about other clients’ changesets
5. Connect to the server and request the initial document

As these 5 events happen, the client updates its representation  A · X · Y
according to the relations that follow. Changes “move left” as time goes by:
into Y when the user types, into X when change sets are submitted to the
server, and into A when the server acknowledges changesets.


### 11.1 New local typing

When a user makes an edit E to the document, the client computes the com-
position (Y · E) and updates its local state, i.e. Y ← Y · E. I.e., if Y is the
variable holding local unsubmitted changes, it will be assigned the new value
(Y · E).

### 11.2 Submitting changesets to server

When a client submit its local changes to the server, it transmits a copy of Y
and then assigns Y to X, and assigns the identity to Y. I.e.,

1. Send Y to server,

```
2. X ← Y
```
```
3. Y ← IN(the identity).
```
This happens every 500ms as long as it receives an acknowledgement. Must
receive ACK before submitting again. Note thatX is always equal to the
identity before the second step occurs, so no information is lost.

### 11.3 Hear ACK from server

When the client hears ACK from server,
A ← A · X
X ← IN

### 11.4 Hear about another client’s changeset

When a client hears about another client’s changeset B, it computes a new A,
X, and Y, which we will call A′, X′, and Y′ respectively. It also computes a
changeset D which is applied to the current text view on the client, V. Because
AXY must always equal the current view, AXY = V before the client hears
about B, and A′X′Y′ = V D after the computation is performed.
The steps are:

1. Compute A′=A · B
2. Compute X′=f(B, X)
3. Compute Y′=f(f(X, B), Y)
4. Compute D=f(Y, f(X, B))
5. Assign A ← A′, X ← X′, Y ← Y′.
6. Apply D to the current view of the document displayed on the user’s
    screen.

```
In steps 2, 3, and 4, f is the follow operation described in Section 8.
```

Proof that AXY = V ⇒ A′X′Y′ = VD. Substituting A′X′Y′ = (AB)(f(B, X))(f(f(X, B), Y)),
we recall that merges are commutative. So for any two changesets P and Q,

```
m(P, Q) = m(Q, P) = Q f(Q, P) = P f(P, Q)
```
```
Applying this to the relation above, we see
``` 
```
A′X′Y′ = ABf(B, X)f(f(X, B), Y)
= AXf(X, B)f(f(X, B), Y)
= AXY f(Y, f(X, B))
= AXY D
= V D
```
As claimed.

### 11.5 Connect to server

When a client connects to the server for the first time, it first generates a random
unique ID and sends this to the server. The client remembers this ID and sends
it with each changeset to the server.
The client receives the latest version of the document from the server, called
HEADTEXT. The client then sets

```
A ← HEADTEXT
```
```
X ← IN
```
```
Y ← IN
```
```
And finally, the client displays HEADTEXT on the screen.
```
## 12 Server Overview

Like the client(s), the server has state and performs operations. Operations are
only performed in response to messages from clients.

## 13 Server State

The server maintains a document as an ordered list ofrevision records. A
revision record is a data structure that contains a changeset and authorship
information.

RevisionRecord = {
ChangeSet,
Source (unique ID),
Revision Number (consecutive order, starting at 0)
}


For efficiency, the server may also store a variable called HEADTEXT, which
is the composition of all changesets in the list of revision records. This is an
optimization, because clearly this can be computed from the set of revision
records.

## 14 Server Operations Overview

The server does two things in addition to maintaining state representing the set
of connected clients and remembering what revision number each client is up to
date with:

1. Respond to a client’s connection requesting the initial document.
2. Respond to a client’s submission of a new changeset.

### 14.1 Respond to client connect

When a server receives a connection request from a client, it receives the client’s
unique ID and stores that in the server’s set of connected clients. It then sends
the client the contents of HEADTEXT, and the corresponding revision number.
Finally the server notes that this client is up to date with that revision number.

### 14.2 Respond to client changeset

When the server receives information from a client about the client’s changeset
C, it does five things:

1. Notes that this change applies to revision number rC (the client’s latest
    revision).
2. Creates a new changeset C′ that is relative to the server’s most recent revi-
    sion number, which we call rH (H for HEAD). C′ can be computed using
    follows (Section 8). Remember that the server has a series of changesets,

```
S 0 →S 1 →... →Src→Src+1→... →SrH
```
```
C is relative to Src, but we need to compute C′ relative to SrH. We can
compute a new C relative to Src+1 by computing f(Src+1, C). Similarly
we can repeat for Src+2 and so forth until we have C′ represented relative
to SrH.
```
3. Send C′ to all other clients
4. Send ACK back to original client
5. Add C′ to the server’s list of revision records by creating a new revision
    record out of this and the client’s ID.


## Additional topics

```
(c) How authorship information is used to color-code the document based
on who typed what
(d) How persistent connections are maintained between client and server
```
etherpad  working described overview:(My Understanding)
To deeply understand Etherpad’s EasySync operational transformation (OT) algorithm, we need to move past the summarized text in the specification and translate it into a mathematically rigorous state machine.

The primary goal of this algorithm is ensuring that when two clients make simultaneous edits (Changesets A and B), we can compute a merged intent $m(A,B)$ , and the corresponding "Follow" transformations $f(A,B)$ and $f(B,A)$. For the system to be stable, two mathematical constraints must absolutely hold true:

Commutativity of Merge: $m(A,B) = m(B,A)$.

Follow Equivalence: $Af(A,B) = Bf(B,A) = m(A,B)$.
Here is the exact, verifiable algorithm that guarantees these properties.


1. The Core Primitives
The PDF represents a changeset as an array of integers (retained characters) and characters (inserted text). Deletions are handled implicitly by simply not retaining a character.


However, to build a deterministic algorithm, we must make the pointers explicit. A Changeset is an ordered list of three primitive operations applied sequentially to the original document:

Retain(n): Keep n characters from the source document.
Insert(string): Insert the literal string at the current position.
Delete(n): Skip n characters from the source document.
Note: Changesets are completely consumed. The sum of all Retain and Delete lengths must exactly equal the length of the original document.

2. The Exact Algorithm (Code Logic)
To compute the Follow $f(A,B)$—which calculates what Client B's changeset should look like given that Client A's changeset has already been applied —we iterate through the operations of both changesets simultaneously.

Because A and B originally applied to the same initial document, their Retain and Delete operations consume the same underlying text.

Python 
def follow(A, B):
"""
Computes B' = f(A, B).
Returns how B should be modified to apply cleanly after A.
"""
iterA = iter(A)
iterB = iter(B)
opA = next(iterA)
opB = next(iterB)
B_prime = []

while opA is not None or opB is not None:
# Case 1: Both insert at the exact same location.
# We need a deterministic tie-breaker to ensure m(A,B) = m(B,A).
if is_insert(opA) and is_insert(opB):
if opA.text <= opB.text:
# A's text goes first. B must skip over A's newly inserted text.
B_prime.append(Retain(length(opA.text)))
opA = next(iterA)
else:
# B's text goes first. B inserts it now.
B_prime.append(Insert(opB.text))
opB = next(iterB)

# Case 2: A inserts text. B knows nothing about it.
# B must simply retain (skip over) A's new text.
elif is_insert(opA):
B_prime.append(Retain(length(opA.text)))
opA = next(iterA)

# Case 3: B inserts text. B's intent is to add it, so it executes.
elif is_insert(opB):
B_prime.append(Insert(opB.text))
opB = next(iterB)

# Case 4: Both operations process the original document (Retain or Delete).
else:
length = min(opA.length, opB.length)

if opA.type == 'Retain' and opB.type == 'Retain':
B_prime.append(Retain(length))

elif opA.type == 'Retain' and opB.type == 'Delete':
# A kept the text, but B wants to delete it. B deletes it now.
B_prime.append(Delete(length))

elif opA.type == 'Delete' and opB.type == 'Retain':
# A already deleted the text. B wanted to keep it, but it's gone.
# B does nothing (the retain is dropped).
pass

elif opA.type == 'Delete' and opB.type == 'Delete':
# Both deleted it. It's already gone.
pass

# Decrement the consumed length and advance if an operation is fully consumed
opA.length -= length
opB.length -= length
if opA.length == 0: opA = next(iterA)
if opB.length == 0: opB = next(iterB)

return B_prime

Why the tie-breaker matters: The PDF states that insertions in A become retains in $f(A,B)$ , and insertions in B become insertions in $f(A,B)$. But if both insert at the exact same index, applying that rule blindly makes the outcome dependent on order. By using a strict lexical sort (opA.text <= opB.text), we guarantee that regardless of whether we compute $f(A,B)$ or $f(B,A)$, "apple" will always be inserted before "banana", preserving commutativity.

3. Step-by-Step Proofs
Let's test this algorithm against progressively harder scenarios to mathematically prove $Af(A,B) = Bf(B,A)$.

1
Level 1: The Easy Case (Non-overlapping inserts)
Base Document: 'doc' (length 3)
Intent: Client A appends "X". Client B appends "Y".

$A = [\text{Retain}(3), \text{Insert}("X")]$
$B = [\text{Retain}(3), \text{Insert}("Y")]$
Trace $f(A,B)$:

Both are Retain(3). B_prime appends Retain(3).
opA is Insert("X"). A inserted, so B must skip it. B_prime appends Retain(1).
opB is Insert("Y"). B inserts. B_prime appends Insert("Y").

Result: $B' = [\text{Retain}(3), \text{Retain}(1), \text{Insert}("Y")] = [\text{Retain}(4), \text{Insert}("Y")]$
Verification: Apply A to "doc" $\rightarrow$ "docX".

Apply $B'$ to "docX" $\rightarrow$ Retain 4 ("docX"), Insert "Y" $\rightarrow$ "docXY".
2
Level 2: The Medium Case (Delete vs Insert)
Base Document: 'car' (length 3)
Intent: Client A deletes "c" (creates "ar"). Client B inserts "t" at the end (creates "cart").

$A = [\text{Delete}(1), \text{Retain}(2)]$
$B = [\text{Retain}(3), \text{Insert}("t")]$
Trace $f(A,B)$:

opA=Delete(1), opB=Retain(3). Overlap is length 1. opA deleted it, opB retained it. B_prime drops the retain (does nothing). opA is consumed. opB becomes Retain(2).
opA=Retain(2), opB=Retain(2). Both retain. B_prime appends Retain(2).
opB=Insert("t"). B_prime appends Insert("t").

Result: $B' = [\text{Retain}(2), \text{Insert}("t")]$
Verification:

Apply A to "car" $\rightarrow$ "ar".

Apply $B'$ to "ar" $\rightarrow$ Retain 2 ("ar"), Insert "t" $\rightarrow$ "art".
3
Level 3: The Complex Case (Simultaneous Conflicting Inserts)
Base Document: 'x' (length 1)
Intent: Both clients insert at the exact start of the document. A inserts "b", B inserts "a".

$A = [\text{Insert}("b"), \text{Retain}(1)]$
$B = [\text{Insert}("a"), \text{Retain}(1)]$
Trace $f(A,B)$:

Both are Insert. We hit the tie-breaker: `"b"
 
The EasySync specification in the pdf given intentionally glosses over the network and failure models to focus on the operational transformation (OT) mathematics. To build a robust system, we must rigidly define the logical clocks (revision numbers), the exact persistence layer, and the network assumptions.

Here is the exact state machine and communication model that powers the architecture.

1. Network Assumptions & Failure Guarantees
EasySync relies heavily on a stateful, connection-oriented protocol (like WebSockets or long-polling XHR over TCP).

Ordering & Delivery: The system assumes that within a single active connection, messages arrive in order and without duplication.
Idempotency / Duplication Protection: If a connection drops while a client is waiting for an ACK, the client does not know if the server received the changeset. When reconnecting, the client must submit a "Reconnect Sync" containing its last known server revision and its unacknowledged X. The server checks its persistent revision log: if it already has a changeset from this client matching the expected state, it drops the duplicate.
Fault Tolerance: The server is the single source of truth. If the server crashes, all connected clients will drop to an offline state. Upon server restart, clients reconnect, download the server's authoritative state (re-establishing their A state), and attempt to merge their local X and Y changes on top of it.
2. The Persistence Model
What is actually saved to disk versus kept in memory is critical for understanding system recovery.

State Element
Location
Persistence
Description
Revision Log
Server
Disk (Persistent)
An append-only list of every RevisionRecord ever accepted. This is the ultimate source of truth.

 
HEADTEXT
Server
Memory (Ephemeral)
The current full text of the document. This is strictly an optimization — it can be rebuilt from scratch by applying the revision log.
A (Acknowledged)
Client
Memory (Ephemeral)
The last known state of the server. Wiped if the user closes the tab.

X (Submitted)
Client
Memory (Ephemeral)
Changes sent but not yet ACKed. Lost if the browser crashes.

Y (Unsubmitted)
Client
Memory (Ephemeral)
Local typing not yet sent. Lost if the browser crashes.

Client ID
Client
Session/Local Storage
A unique ID generated on first connect. Allows the server to track authorship and drop echoed changes.

Offline capabilities: Standard EasySync is an online-first algorithm. Because X and Y are ephemeral, closing the browser before receiving an ACK permanently destroys those unsubmitted edits. Modern implementations often write X and Y to localStorage to survive page reloads, though this requires complex deduplication logic upon reconnection. may real etherpad might use some complex things to be offilne first i guess i definetly dont know.

3. The Exact State Machine
To mathematically verify the communication, we must define the exact state variables and the routing logic for both the Client and the Server.

The Client Node
The client tracks the server_rev to tell the server exactly which version of the document X was based on.

Python 
class Client:
def __init__(self, document_text, initial_rev):
self.client_id = generate_uuid() #
self.A = text_to_changeset(document_text) # [cite: 84]
self.X = IdentityChangeset() # [cite: 86]
self.Y = IdentityChangeset() # [cite: 88]
self.server_rev = initial_rev
self.waiting_for_ack = False

def local_typing(self, edit_E):
# User types on screen. Fold it into Y.
self.Y = compose(self.Y, edit_E) # [cite: 100]

def submit_to_server(self, network):
if self.waiting_for_ack or is_identity(self.Y):
return

# Shift Y to X, reset Y [cite: 104, 105]
self.X = self.Y
self.Y = IdentityChangeset()
self.waiting_for_ack = True

# CRITICAL: Send the revision number A is based on
network.send('SUBMIT', {
'client_id': self.client_id, # [cite: 135]
'base_rev': self.server_rev,
'changeset': self.X
})

def receive_ack(self):
# The server accepted X. It is now part of the canonical history.
self.A = compose(self.A, self.X) # [cite: 109]
self.X = IdentityChangeset() # [cite: 110]
self.server_rev += 1
self.waiting_for_ack = False

def receive_broadcast(self, incoming_changeset, new_rev):
B = incoming_changeset

# 1. Update A: The server accepted B.
A_prime = compose(self.A, B) # [cite: 115]

# 2. Update X: How does X change now that B happened before it?
X_prime = follow(B, self.X) # [cite: 116]

# 3. Update Y: How does Y change now that both B and X happened?
Y_prime = follow(follow(self.X, B), self.Y) # [cite: 117]

# Apply the visual shift to the user's screen
D = follow(self.Y, follow(self.X, B)) # [cite: 118]
apply_to_screen(D) # [cite: 120]

self.A, self.X, self.Y = A_prime, X_prime, Y_prime # [cite: 119]
self.server_rev = new_rev

The Server Node
The server's primary job is Operational Transformation routing: catching an old changeset up to the present.

Python 
class Server:
def __init__(self, initial_text):
self.head_text = initial_text #
self.revisions = [] # Append-only log of RevisionRecords
self.head_rev = 0

def handle_submit(self, client_id, base_rev, client_changeset):
# If base_rev == self.head_rev, no transformation is needed.
# But if base_rev < self.head_rev, other clients have submitted changes
# while this client was typing. We must catch C up to HEAD[cite: 168].

C_prime = client_changeset

# Iterate through every revision that happened after the client's base_rev
for r in range(base_rev, self.head_rev):
# Fetch the historical changeset from the persistent log [cite: 169]
historical_change = self.revisions[r].changeset

# Transform the client's change against the historical change [cite: 170]
C_prime = follow(historical_change, C_prime)

# C_prime is now relative to self.head_rev. We can safely apply it.
self.head_text = apply_changeset(self.head_text, C_prime)

# Persist the new state [cite: 174]
self.revisions.append(RevisionRecord(
changeset=C_prime,
source=client_id, # [cite: 149]
rev_number=self.head_rev # [cite: 150]
))

self.head_rev += 1

# Send ACK to the author [cite: 173]
network.send_to(client_id, 'ACK')

# Broadcast C_prime (the transformed change) to everyone else [cite: 172]
network.broadcast_except(client_id, 'BROADCAST', {
'changeset': C_prime,
'new_rev': self.head_rev
})


4. The Critical Race Condition Resolved
Notice the asymmetry here: m(A,B) (the merge intent) is rarely calculated explicitly on the server. The algorithm strictly uses follow(A, B).

When Client 1 and Client 2 submit simultaneously, the network dictates who "wins" the race to the server. If Client 1's packet arrives 1 millisecond faster:

Server accepts Client 1's change (no transformation needed). Server increments to Revision 1.
Server broadcasts Revision 1 to Client 2.
Server receives Client 2's packet, which is based on Revision 0.
Server runs follow(Client 1's change, Client 2's change) to generate C_prime.
Server saves C_prime as Revision 2, ACKs Client 2, and broadcasts Revision 2.
Because the math guarantees $Af(A,B) = Bf(B,A)$, both clients end up with the exact same final document, despite processing the operations in different orders locally.

So if you see what Etherpad is built for, you will understand that they only run one single server and multiple clients. And if the server fails, then all the clients enter offline editing mode, so they can always type locally whatever happens to server, where after the server is up any time in the future, they will again connect to it and send those local changes, but they are always guaranteed to be consistent I guess.
