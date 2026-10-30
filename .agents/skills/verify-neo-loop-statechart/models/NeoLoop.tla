---- MODULE NeoLoop ----
EXTENDS Naturals
CONSTANTS ReplanValidNext, ReplayUnknown, SkipAcceptance, LegacyArchiveGate, ResolveKnownRejection
VARIABLES phase, validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted
vars == <<phase, validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>
Init ==
    /\ phase = "Owner"
    /\ validNext \in BOOLEAN
    /\ frozen \in BOOLEAN
    /\ slots \in {0, 1}
    /\ unknown = FALSE
    /\ replayed = FALSE
    /\ verified = FALSE
    /\ accepted = FALSE
    /\ commit = FALSE
    /\ goalKept = TRUE
    /\ delta = FALSE
    /\ restarted = FALSE

UseOriginalNext ==
    /\ phase = "Owner" /\ validNext
    /\ phase' = "Admission"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

AskMaterialQuestion ==
    /\ phase = "Owner" /\ ~validNext
    /\ phase' = "Thinking"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

UnnecessaryReplan ==
    /\ phase = "Owner" /\ validNext /\ ReplanValidNext
    /\ phase' = "Thinking"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

ResolvePremise ==
    /\ phase = "Thinking"
    /\ phase' = "Admission"
    /\ validNext' = TRUE
    /\ UNCHANGED <<slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

WaitForQuota ==
    /\ phase = "Admission" /\ slots = 0
    /\ phase' = "QuotaWait"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

ReadQuota ==
    /\ phase = "QuotaWait"
    /\ phase' = "Admission"
    /\ slots' = 1
    /\ UNCHANGED <<validNext, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

ObserveRestart ==
    /\ phase = "Admission" /\ ~restarted
    /\ phase' = "Recovery"
    /\ restarted' = TRUE
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta>>

RetiredArchiveGate ==
    /\ phase = "Recovery" /\ LegacyArchiveGate
    /\ phase' = "ArchiveBlocked"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

RecoverByOwner ==
    /\ phase = "Recovery" /\ ~LegacyArchiveGate
    /\ phase' = "Admission"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

Dispatch ==
    /\ phase = "Admission" /\ slots = 1
    /\ phase' = "Running"
    /\ slots' = 0
    /\ UNCHANGED <<validNext, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

CaptureCandidate ==
    /\ phase = "Running"
    /\ phase' = "Candidate"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

LoseEffectReadback ==
    /\ phase = "Running"
    /\ phase' = "Unknown"
    /\ unknown' = TRUE
    /\ UNCHANGED <<validNext, slots, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

ReadOriginalEffect ==
    /\ phase = "Unknown"
    /\ phase' = "Candidate"
    /\ unknown' = FALSE
    /\ UNCHANGED <<validNext, slots, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

ReadRejectedAdmission ==
    /\ phase = "Unknown" /\ ResolveKnownRejection
    /\ phase' = "Recovery"
    /\ unknown' = FALSE
    /\ UNCHANGED <<validNext, slots, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

UnsafeReplay ==
    /\ phase = "Unknown" /\ ReplayUnknown
    /\ phase' = "Running"
    /\ replayed' = TRUE
    /\ UNCHANGED <<validNext, slots, unknown, verified, accepted, commit, goalKept, frozen, delta, restarted>>

IndependentTest ==
    /\ phase = "Candidate"
    /\ phase' = "Reviewed"
    /\ verified' = TRUE
    /\ UNCHANGED <<validNext, slots, unknown, replayed, accepted, commit, goalKept, frozen, delta, restarted>>

FindCounterexample ==
    /\ phase = "Candidate"
    /\ phase' = "Repair"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

HoldFrozenFeedback ==
    /\ phase = "Repair" /\ frozen
    /\ phase' = "RevisionBarrier"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

OriginalLegalRevision ==
    /\ phase = "RevisionBarrier"
    /\ phase' = "Repair"
    /\ frozen' = FALSE
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, delta, restarted>>

RepairEarliestProducer ==
    /\ phase = "Repair" /\ ~frozen
    /\ phase' = "Owner"
    /\ delta' = TRUE
    /\ validNext' = TRUE
    /\ slots' = 1
    /\ UNCHANGED <<unknown, replayed, verified, accepted, commit, goalKept, frozen, restarted>>

IntegrateCommit ==
    /\ phase = "Reviewed"
    /\ phase' = "Integrated"
    /\ commit' = TRUE
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, goalKept, frozen, delta, restarted>>

OwnerAcceptance ==
    /\ phase = "Integrated" /\ verified
    /\ phase' = "Accepted"
    /\ accepted' = TRUE
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, commit, goalKept, frozen, delta, restarted>>

Deliver ==
    /\ phase = "Accepted" /\ commit /\ accepted
    /\ phase' = "Delivered"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

SkipIndependentAcceptance ==
    /\ phase = "Integrated" /\ SkipAcceptance
    /\ phase' = "Delivered"
    /\ UNCHANGED <<validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

End ==
    /\ phase \in {"Delivered", "ArchiveBlocked", "Unknown"}
    /\ UNCHANGED <<phase, validNext, slots, unknown, replayed, verified, accepted, commit, goalKept, frozen, delta, restarted>>

Next ==
    UseOriginalNext
    \/ AskMaterialQuestion
    \/ UnnecessaryReplan
    \/ ResolvePremise
    \/ WaitForQuota
    \/ ReadQuota
    \/ ObserveRestart
    \/ RetiredArchiveGate
    \/ RecoverByOwner
    \/ Dispatch
    \/ CaptureCandidate
    \/ LoseEffectReadback
    \/ ReadOriginalEffect
    \/ ReadRejectedAdmission
    \/ UnsafeReplay
    \/ IndependentTest
    \/ FindCounterexample
    \/ HoldFrozenFeedback
    \/ OriginalLegalRevision
    \/ RepairEarliestProducer
    \/ IntegrateCommit
    \/ OwnerAcceptance
    \/ Deliver
    \/ SkipIndependentAcceptance
    \/ End

GoalPreserved == goalKept
NoUnknownReplay == ~replayed
NoRetiredArchiveGate == phase # "ArchiveBlocked"
NoUnnecessaryThinking == (phase = "Thinking") => ~validNext
DeliveryNeedsAcceptance == (phase = "Delivered") => (verified /\ accepted /\ commit)
Spec == Init /\ [][Next]_vars
====
