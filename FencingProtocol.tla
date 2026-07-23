------------------------------ MODULE FencingProtocol ------------------------------
(***************************************************************************)
(* Fencing protocol for the "sleeping cloud": single-writer SQLite per    *)
(* app, enforced across snapshot/restore, lease expiry, partitions, and   *)
(* arbitrary process pause.                                               *)
(*                                                                        *)
(* This module is the machine-checkable companion to the design docs:     *)
(*                                                                        *)
(*   Epoch rules   E1..E6  -> actions below (marked in comments)          *)
(*   Invariants    I1..I4  -> EpochsMonotoneInWAL, AckedDurable +         *)
(*                            WriterAtHead, SnapshotsOnHistory,           *)
(*                            OneInstancePerEpoch                         *)
(*   Pressure tests PT-1..3 -> reproducible as counterexample traces by   *)
(*                            flipping the CONSTANT toggles (see below)   *)
(*                                                                        *)
(* THE MODEL, IN ONE PARAGRAPH                                            *)
(* One app. A set of host agents compete to run its single instance. A   *)
(* control plane grants leases and mints monotone epochs (E1). The WAL   *)
(* is a sequence of epoch stamps: wal[i] is the epoch under which LSN i  *)
(* was accepted by the gateway; `curEpoch` is the sealed/current epoch   *)
(* recorded in the object-store manifest (E4). Durability = presence in  *)
(* wal; a write is acked to the client only on acceptance (contract D1). *)
(* Snapshots are (epoch, lsn) manifests; registration is fenced (E5).    *)
(* Restore replays the WAL delta from the snapshot's LSN to head before  *)
(* the instance serves (E6).                                             *)
(*                                                                        *)
(* WHY THERE IS NO Pause ACTION                                           *)
(* "A VM can freeze for three weeks and resume believing it holds the    *)
(* lease" is the platform's defining hazard (PT-3).  In an interleaved   *)
(* asynchronous spec, arbitrary pause needs no explicit action: any      *)
(* schedule in which a host takes no steps for a while IS a pause, and   *)
(* TLC explores all of them -- including "lease expires and a new epoch  *)
(* takes over while the old holder is frozen mid-anything".  Pause would  *)
(* only matter for liveness (fairness assumptions), which this safety    *)
(* model deliberately leaves to the deterministic-simulation harness.    *)
(*                                                                        *)
(* HOW TO RUN                                                             *)
(*   java -cp tla2tools.jar tlc2.TLC -config FencingProtocol.cfg \        *)
(*        FencingProtocol.tla                                             *)
(* or open the folder with the VS Code TLA+ extension and "Check model    *)
(* with TLC".  With the default constants (2 hosts, 3 epochs, 3 writes)   *)
(* the state space is small; TLC finishes in seconds.                     *)
(*                                                                        *)
(* NEGATIVE TESTS -- make TLC produce the pressure-test traces.  All      *)
(* three toggles are load-bearing; each was verified to fail when off:    *)
(*   GatewayFencing  = FALSE  ->  violates WriterAtHead (and, on longer   *)
(*       traces, EpochsMonotoneInWAL): the trace is PT-1, a partition     *)
(*       zombie appending old-epoch WAL after a successor sealed.         *)
(*   ReplayOnRestore = FALSE  ->  violates WriterAtHead: the E6 bug --    *)
(*       an instance serving from a stale snapshot without replaying the  *)
(*       delta (the second fence of PT-2).                                *)
(*   RegistrationFencing = FALSE -> violates SnapshotsOnHistory via a     *)
(*       trace the hand-written pressure tests MISSED -- the "slow        *)
(*       waker": a host acquires epoch e, stalls BEFORE rehydrating, is   *)
(*       superseded by epoch e+1 which seals and writes; the stalled      *)
(*       host then rehydrates (replaying the successor's data into        *)
(*       itself), goes active, snapshots, and registers a manifest        *)
(*       claiming epoch-e lineage over epoch-(e+1) data.  The manifest    *)
(*       lies about ancestry, which poisons PITR branching and GC.  The   *)
(*       slow waker never attempts a WAL append, so E3 never gets a       *)
(*       chance to catch it -- E5 is the ONLY fence on this path.  This   *)
(*       finding was produced by TLC, not by the human review; the        *)
(*       original draft of this header predicted E5-off would be safe.    *)
(*                                                                        *)
(* SCOPE LIMITS (deliberate)                                              *)
(*   - LSNs are abstract; SQLite checkpoint content is not modeled.       *)
(*   - One cell: the (cell_generation ‖ counter) composite epoch of E1    *)
(*     collapses to a plain counter here; cell failover fencing would be  *)
(*     a second module reusing the same shape.                            *)
(*   - Reads are not modeled; bounded stale reads (PT-6) are a documented *)
(*     product property, not a safety invariant.                          *)
(***************************************************************************)

EXTENDS Integers, Sequences, FiniteSets

CONSTANTS
  Hosts,                \* set of host agents competing for the app
  None,                 \* model value: "nobody holds the lease"
  MaxEpoch,             \* model-checking bound on lease acquisitions
  MaxWrites,            \* model-checking bound on WAL length
  GatewayFencing,       \* E3/E4: gateway rejects stale-epoch segments
  RegistrationFencing,  \* E5:    control plane rejects stale-epoch manifests
  ReplayOnRestore       \* E6:    restore replays WAL delta before serving

ASSUME None \notin Hosts

VARIABLES
  epoch,        \* control plane: last epoch minted (E1)
  leaseHolder,  \* control plane: current holder, or None; expiry is silent
  curEpoch,     \* gateway/manifest: sealed current epoch (E4's CAS state)
  wal,          \* durable history: wal[i] = epoch that wrote LSN i
  acked,        \* LSNs acknowledged to the client (contract D1)
  snaps,        \* registered snapshot manifests: {[e |-> epoch, lsn |-> lsn]}
  hstate,       \* per host: IDLE / REHYDRATING / ACTIVE / SEALING / DEAD
  hepoch,       \* per host: epoch this instance believes it holds
  hlsn,         \* per host: last LSN this instance has applied
  pending       \* per host: manifest captured at BeginSeal, awaiting E5 check

vars == <<epoch, leaseHolder, curEpoch, wal, acked, snaps,
          hstate, hepoch, hlsn, pending>>

LiveStates == {"REHYDRATING", "ACTIVE", "SEALING"}
head       == Len(wal)
Manifests  == [e : 0..MaxEpoch, lsn : 0..MaxWrites]

TypeOK ==
  /\ epoch \in 0..MaxEpoch
  /\ curEpoch \in 0..MaxEpoch
  /\ leaseHolder \in Hosts \cup {None}
  /\ wal \in Seq(1..MaxEpoch)
  /\ acked \subseteq 1..MaxWrites
  /\ snaps \subseteq Manifests
  /\ hstate \in [Hosts -> {"IDLE", "REHYDRATING", "ACTIVE", "SEALING", "DEAD"}]
  /\ hepoch \in [Hosts -> 0..MaxEpoch]
  /\ hlsn \in [Hosts -> 0..MaxWrites]
  /\ pending \in [Hosts -> Manifests]

Init ==
  /\ epoch = 0
  /\ leaseHolder = None
  /\ curEpoch = 0
  /\ wal = <<>>
  /\ acked = {}
  /\ snaps = {[e |-> 0, lsn |-> 0]}          \* deploy-time empty snapshot
  /\ hstate = [h \in Hosts |-> "IDLE"]
  /\ hepoch = [h \in Hosts |-> 0]
  /\ hlsn = [h \in Hosts |-> 0]
  /\ pending = [h \in Hosts |-> [e |-> 0, lsn |-> 0]]

(***************************************************************************)
(* E1: epochs are minted by a CAS in the control plane's linearizable     *)
(* store, strictly monotone, one instance per epoch.                      *)
(***************************************************************************)
AcquireLease(h) ==
  /\ hstate[h] = "IDLE"
  /\ leaseHolder = None
  /\ epoch < MaxEpoch
  /\ epoch' = epoch + 1
  /\ leaseHolder' = h
  /\ hepoch' = [hepoch EXCEPT ![h] = epoch + 1]
  /\ hstate' = [hstate EXCEPT ![h] = "REHYDRATING"]
  /\ UNCHANGED <<curEpoch, wal, acked, snaps, hlsn, pending>>

(***************************************************************************)
(* E6: on restore, seal the stream at the new epoch and replay the WAL    *)
(* delta from the chosen snapshot's LSN to head BEFORE serving.  Note     *)
(* that WHICH snapshot is picked is irrelevant precisely because replay   *)
(* covers the delta -- flip ReplayOnRestore to FALSE and watch TLC show   *)
(* why the replay is load-bearing.  The seal here is the "greater epoch   *)
(* seals its predecessor" clause of E3, performed at takeover.            *)
(***************************************************************************)
Rehydrate(h) ==
  /\ hstate[h] = "REHYDRATING"
  /\ \E s \in snaps :
       hlsn' = [hlsn EXCEPT ![h] = IF ReplayOnRestore THEN head ELSE s.lsn]
  /\ curEpoch' = IF GatewayFencing /\ hepoch[h] > curEpoch
                   THEN hepoch[h] ELSE curEpoch
  /\ hstate' = [hstate EXCEPT ![h] = "ACTIVE"]
  /\ UNCHANGED <<epoch, leaseHolder, wal, acked, snaps, hepoch, pending>>

(***************************************************************************)
(* E2 + E3 + D1: the host agent stamps the segment with its epoch; the    *)
(* gateway accepts iff the stamp is >= the sealed current epoch (sealing  *)
(* forward if greater); the client ack happens at acceptance and never    *)
(* before.  With GatewayFencing = FALSE the gateway is an unfenced sink   *)
(* and TLC reproduces PT-1.                                               *)
(***************************************************************************)
WriteOK(h) ==
  /\ hstate[h] = "ACTIVE"
  /\ head < MaxWrites
  /\ (~GatewayFencing) \/ (hepoch[h] >= curEpoch)
  /\ wal' = Append(wal, hepoch[h])
  /\ curEpoch' = IF GatewayFencing /\ hepoch[h] > curEpoch
                   THEN hepoch[h] ELSE curEpoch
  /\ hlsn' = [hlsn EXCEPT ![h] = head + 1]
  /\ acked' = acked \cup {head + 1}
  /\ UNCHANGED <<epoch, leaseHolder, snaps, hstate, hepoch, pending>>

(***************************************************************************)
(* E3, rejection path: a fenced-out segment tells the host agent its      *)
(* instance is a zombie; it kills the instance and discards local state.  *)
(* (PT-1's resolution.)                                                   *)
(***************************************************************************)
WriteFenced(h) ==
  /\ hstate[h] = "ACTIVE"
  /\ GatewayFencing
  /\ hepoch[h] < curEpoch
  /\ hstate' = [hstate EXCEPT ![h] = "DEAD"]
  /\ UNCHANGED <<epoch, leaseHolder, curEpoch, wal, acked, snaps,
                 hepoch, hlsn, pending>>

(***************************************************************************)
(* Checkpoint-then-snapshot: the manifest captures (epoch, applied LSN)   *)
(* at the moment of seal.  A slow waker that replayed a successor's       *)
(* writes will capture a manifest whose epoch lies about the lineage of   *)
(* its prefix -- E5 at RegisterOK is the only fence that catches it (see  *)
(* the "slow waker" note in the header).                                  *)
(***************************************************************************)
BeginSeal(h) ==
  /\ hstate[h] = "ACTIVE"
  /\ pending' = [pending EXCEPT ![h] = [e |-> hepoch[h], lsn |-> hlsn[h]]]
  /\ hstate' = [hstate EXCEPT ![h] = "SEALING"]
  /\ UNCHANGED <<epoch, leaseHolder, curEpoch, wal, acked, snaps,
                 hepoch, hlsn>>

(* E5: snapshot registration is a fenced CAS at the control plane. *)
RegisterOK(h) ==
  /\ hstate[h] = "SEALING"
  /\ (~RegistrationFencing) \/ (hepoch[h] = epoch)
  /\ snaps' = snaps \cup {pending[h]}
  /\ leaseHolder' = IF leaseHolder = h THEN None ELSE leaseHolder
  /\ hstate' = [hstate EXCEPT ![h] = "IDLE"]
  /\ UNCHANGED <<epoch, curEpoch, wal, acked, hepoch, hlsn, pending>>

RegisterFenced(h) ==
  /\ hstate[h] = "SEALING"
  /\ RegistrationFencing
  /\ hepoch[h] # epoch
  /\ hstate' = [hstate EXCEPT ![h] = "DEAD"]
  /\ UNCHANGED <<epoch, leaseHolder, curEpoch, wal, acked, snaps,
                 hepoch, hlsn, pending>>

(***************************************************************************)
(* The TTL fires (or a partition makes it fire).  Crucially the holder    *)
(* is NOT notified -- its hstate stays ACTIVE.  This single action is     *)
(* what creates every zombie in the model.                                *)
(***************************************************************************)
LeaseExpire ==
  /\ leaseHolder # None
  /\ leaseHolder' = None
  /\ UNCHANGED <<epoch, curEpoch, wal, acked, snaps,
                 hstate, hepoch, hlsn, pending>>

(* Instance dies abruptly; the control plane learns only via expiry. *)
Crash(h) ==
  /\ hstate[h] \in LiveStates
  /\ hstate' = [hstate EXCEPT ![h] = "DEAD"]
  /\ UNCHANGED <<epoch, leaseHolder, curEpoch, wal, acked, snaps,
                 hepoch, hlsn, pending>>

(* The agent machine starts a fresh instance slot. *)
Restart(h) ==
  /\ hstate[h] = "DEAD"
  /\ hstate' = [hstate EXCEPT ![h] = "IDLE"]
  /\ hepoch' = [hepoch EXCEPT ![h] = 0]
  /\ hlsn' = [hlsn EXCEPT ![h] = 0]
  /\ pending' = [pending EXCEPT ![h] = [e |-> 0, lsn |-> 0]]
  /\ UNCHANGED <<epoch, leaseHolder, curEpoch, wal, acked, snaps>>

Next ==
  \/ LeaseExpire
  \/ \E h \in Hosts :
       \/ AcquireLease(h)
       \/ Rehydrate(h)
       \/ WriteOK(h)
       \/ WriteFenced(h)
       \/ BeginSeal(h)
       \/ RegisterOK(h)
       \/ RegisterFenced(h)
       \/ Crash(h)
       \/ Restart(h)

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(*                              INVARIANTS                                 *)
(***************************************************************************)

(* I1 -- single writer: once an epoch is superseded in the stream, no     *)
(* lower epoch ever appends again.  Equivalently: epoch stamps in the WAL *)
(* are non-decreasing.                                                    *)
EpochsMonotoneInWAL ==
  \A i \in 1..head : (i + 1 <= head) => (wal[i] <= wal[i + 1])

(* I2, part 1 -- no lost ack: every acknowledged LSN is in the durable    *)
(* history.  (The WAL is append-only in this model, so presence is        *)
(* permanence.)                                                           *)
AckedDurable ==
  \A l \in acked : l \in 1..head

(* I2, part 2 / I4 -- the current-epoch writer is always at head: it saw *)
(* every acknowledged write before producing new ones.  This is the      *)
(* invariant that dies when ReplayOnRestore (E6) is turned off.          *)
WriterAtHead ==
  \A h \in Hosts :
    (hstate[h] = "ACTIVE" /\ hepoch[h] >= curEpoch) => (hlsn[h] = head)

(* I3 -- snapshot lineage: every registered manifest points into the     *)
(* accepted history, and the prefix it claims was written entirely by    *)
(* epochs no newer than its own.  No registered snapshot can fork the    *)
(* stream.                                                               *)
SnapshotsOnHistory ==
  \A s \in snaps :
    /\ s.lsn <= head
    /\ \A i \in 1..s.lsn : wal[i] <= s.e

(* E1 sanity: at most one live instance per minted epoch, ever.          *)
OneInstancePerEpoch ==
  Cardinality({h \in Hosts :
                 hstate[h] \in LiveStates /\ hepoch[h] = epoch
                 /\ hepoch[h] > 0}) <= 1

(* E4 sanity: the manifest's sealed epoch never runs ahead of the        *)
(* control plane's mint counter.                                         *)
CurEpochBounded ==
  curEpoch <= epoch

(***************************************************************************)
(* Liveness is intentionally out of scope: "a wake eventually serves"     *)
(* requires fairness assumptions that hide exactly the schedules this     *)
(* model exists to explore (indefinite pause).  Progress under fault      *)
(* injection belongs to the deterministic-simulation harness, not TLC.    *)
(***************************************************************************)

=============================================================================
