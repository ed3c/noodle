"""Single-driver delegation contract; no model, VM, credential or daemon implementation."""
import hashlib
import json
import math
import os
import threading
import time
from dataclasses import asdict, dataclass, replace
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Protocol


class Refused(RuntimeError):
    """No new native effect was started."""


class UnknownEffect(RuntimeError):
    """Original owner effects are unresolved; every route is closed."""


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def sha256(path):
    if path.is_symlink() or not path.is_file():
        raise Refused("evidence is not a regular file")
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def tree_ref(root):
    """Bind complete explicit context bytes/modes; never follow symlinks."""
    root = Path(root)
    if root.resolve() != root.absolute() or not root.is_dir():
        raise Refused("context is not a canonical directory")
    rows = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise Refused("context contains a symlink")
        if path.is_dir():
            continue
        rows.append([path.relative_to(root).as_posix(), path.stat().st_mode & 0o777,
                     sha256(path)])
    return {"path": str(root), "sha256": hashlib.sha256(canonical(rows)).hexdigest()}


@dataclass(frozen=True)
class OwnerCall:
    group: str
    run_id: str
    role: str
    provider: str
    model: str
    inputs: dict
    # An authorized accounting hold, possibly the full remainder; not a provider cap.
    paid_reservation_usd: str | None = None

    def fingerprint(self):
        return hashlib.sha256(canonical(asdict(self))).hexdigest()


@dataclass(frozen=True)
class OwnerResult:
    run_dir: str
    output_sha256: str
    reply: str
    workspace: str | None
    receipt_path: str
    receipt_sha256: str
    # True requires the real bridge's original-owner completion/publication/Stop proof.
    settled: bool
    cost_usd: str | None
    wall_s: float | None
    cached: bool = False
    billing_receipt: dict | None = None


class OwnerBridge(Protocol):
    def execute(self, call: OwnerCall) -> OwnerResult:
        """Once-only delegate to the existing owner; never start a replacement owner.

        Return only after original completion, validated artifact publication and native
        stopped proof. Raise Refused ONLY when no native effect began; any other exception
        is unknown. This protocol is not an implementation of the Soodles lifecycle.
        """


def money(value):
    if not isinstance(value, str):
        raise Refused("paid amount is not an explicit decimal string")
    try:
        amount = Decimal(value)
    except (InvalidOperation, TypeError, ValueError) as exc:
        raise Refused("paid amount is invalid") from exc
    if not amount.is_finite() or amount < 0:
        raise Refused("paid amount is invalid")
    return amount


class SharedGate:
    """One experiment driver's durable admission journal and serial owner delegation.

    All solver/proposer objects share THIS object. No multi-process filesystem CAS is
    claimed. Reopening pending/unknown evidence refuses; it never replays effects.
    """
    def __init__(self, journal: Path, owner: OwnerBridge | None, *, initial_paid_spent_usd: str):
        self.journal = Path(journal)
        self.owner = owner
        self.lock = threading.RLock()
        initial = str(money(initial_paid_spent_usd))
        self.state = {"version": 1, "paid_limit_usd": "5", "initial_paid_spent_usd": initial,
                      "blocked": "", "paid_blocked": "", "calls": {}}
        if self.journal.exists():
            loaded = json.loads(self.journal.read_text())
            if (loaded["version"] != 1 or loaded["paid_limit_usd"] != "5"
                    or loaded["initial_paid_spent_usd"] != initial):
                raise Refused("original budget binding differs")
            self.state = loaded
            if any(row["state"] in {"pending", "unknown"} for row in loaded["calls"].values()):
                self.state["blocked"] = "original owner effect is unresolved"
        else:
            self.journal.parent.mkdir(parents=True, exist_ok=True)
            self._save()

    def _save(self):
        temporary = self.journal.with_suffix(self.journal.suffix + ".pending")
        with temporary.open("x") as stream:
            stream.write(json.dumps(self.state, indent=2, allow_nan=False) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, self.journal)
        descriptor = os.open(self.journal.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def _spent(self):
        total = money(self.state["initial_paid_spent_usd"])
        for row in self.state["calls"].values():
            if row["provider"] != "pi" or row["state"] == "refused":
                continue
            cost = (row.get("result") or {}).get("cost_usd")
            total += money(cost if cost is not None else row["reserved_usd"])
        return total

    def verify_result(self, result, call=None):
        with self.lock:
            try:
                self._verify_result(result)
                if result.billing_receipt is not None:
                    from openrouter_billing import verify_settlement
                    bill = verify_settlement(call, result, result.billing_receipt)
                    if bill["cost_usd"] != result.cost_usd:
                        raise UnknownEffect("original settled cost differs from billing receipt")
            except (Refused, UnknownEffect, OSError, ValueError) as exc:
                self.state["blocked"] = "original owner completion evidence is unresolved"
                self._save()
                raise UnknownEffect(self.state["blocked"]) from exc

    def _verify_result(self, result):
        if not result.settled:
            raise UnknownEffect("owner completion is unresolved")
        if (tree_ref(Path(result.run_dir))["sha256"] != result.output_sha256
                or not Path(result.run_dir).is_dir()
                or sha256(Path(result.receipt_path)) != result.receipt_sha256
                or (result.workspace is not None and (not Path(result.workspace).is_dir()
                    or not Path(result.workspace).resolve().is_relative_to(Path(result.run_dir).resolve())))):
            raise UnknownEffect("settled owner evidence differs")
        if result.cost_usd is not None:
            try:
                money(result.cost_usd)
            except Refused as exc:
                raise UnknownEffect("owner cost evidence is invalid") from exc
        if result.wall_s is not None and (result.wall_s < 0 or not math.isfinite(result.wall_s)):
            raise UnknownEffect("owner duration is invalid")

    def _require_unique_bill(self, key, bill):
        from openrouter_billing import verify_settlement
        claimed = set(bill["generation_ids"])
        for other_key, other in self.state["calls"].items():
            other_result = other.get("result") or {}
            if other_key == key or other_result.get("billing_receipt") is None:
                continue
            prior = verify_settlement(None, OwnerResult(**other_result), other_result["billing_receipt"])
            if claimed.intersection(prior["generation_ids"]):
                raise Refused("generation bill already belongs to another owner call")

    def reconcile_billing(self, call: OwnerCall, receipt: dict) -> OwnerResult:
        """Settle only an existing completed native call; never invoke its owner again."""
        from openrouter_billing import verify_settlement
        with self.lock:
            key = json.dumps([call.group, call.run_id])
            row = self.state["calls"].get(key)
            if (self.state["blocked"] or row is None or row["state"] != "settled"
                    or row["input_sha256"] != call.fingerprint() or call.provider != "pi"):
                raise Refused("original paid native call has no settled reusable result")
            result = OwnerResult(**row["result"])
            self.verify_result(result, call)
            bill = verify_settlement(call, result, receipt)
            if result.billing_receipt is not None:
                if result.billing_receipt != receipt:
                    raise Refused("original paid call already has a different settlement")
                return result
            self._require_unique_bill(key, bill)
            updated = replace(result, cost_usd=bill["cost_usd"], billing_receipt=receipt)
            row["result"] = asdict(updated)
            if money(updated.cost_usd) > money(row["reserved_usd"]) or self._spent() > Decimal("5"):
                self.state["paid_blocked"] = "actual paid cost exceeded authorized accounting budget"
            try:
                self._save()
            except OSError as exc:
                self.state["blocked"] = "billing settlement persistence is unresolved"
                raise UnknownEffect(self.state["blocked"]) from exc
            return updated

    def reconcile_original_call(self, call: OwnerCall, result: OwnerResult, recovery_ref: dict) -> None:
        """Explicit same-call settlement; retain the unknown journal and never invoke owner."""
        from soodles_bridge import (
            decode,
            host_identity,
            read_ref,
            verify_call_reconciliation,
        )
        with self.lock:
            key = json.dumps([call.group, call.run_id])
            row = self.state["calls"].get(key)
            unresolved = [value for value in self.state['calls'].values() if value['state'] in {'pending', 'unknown'}]
            if (len(unresolved) != 1 or row is None or row['state'] != 'unknown'
                    or row['input_sha256'] != call.fingerprint() or row.get('error') != 'UnknownEffect'):
                raise Refused('journal does not bind this sole unresolved original call')
            for other in self.state['calls'].values():
                if other['state'] == 'settled':
                    self.verify_result(OwnerResult(**other['result']), OwnerCall(**other['call']))
            proof = decode(read_ref(recovery_ref))
            expected_host = verify_call_reconciliation(call, proof)
            self._verify_result(result)
            if proof["result_receipt"] != {"path": result.receipt_path, "sha256": result.receipt_sha256}:
                raise Refused("recovered result receipt differs")
            completed = decode(read_ref(proof["completed_observation"]))
            settled = decode(read_ref(proof["result_receipt"]))
            inner = completed.get("inner") or {}
            progress = inner.get("review_progress") or {}
            selection = progress.get("review") or {}
            if (host_identity(completed) != expected_host or inner.get("owner") != "issue-execution.local-task"
                    or progress.get("status") != "locally_integrated" or inner.get("acceptance") != "PASS"
                    or selection.get("decision") != "approve" or selection.get("candidate") != settled["candidate"]
                    or settled["request"] != proof["request"] or settled["call_sha256"] != call.fingerprint()
                    or settled["original_review"] != proof["completed_observation"]):
                raise Refused("original recovered candidate has no bound integrated acceptance")
            original = self.journal.read_bytes()
            digest = hashlib.sha256(original).hexdigest()
            archive = self.journal.with_name(self.journal.name + ".unknown-" + digest + ".json")
            with archive.open("xb") as stream:
                stream.write(original)
                stream.flush()
                os.fsync(stream.fileno())
            if result.billing_receipt is not None:
                from openrouter_billing import verify_settlement
                bill = verify_settlement(call, result, result.billing_receipt)
                self._require_unique_bill(key, bill)
            prior_row = json.loads(canonical(row))
            history = list(row.get('reconciliations', []))
            history.append({"kind": "original-call-readback", "prior_row": prior_row,
                "prior_journal": {"path": str(archive), "sha256": digest}, "recovery": recovery_ref})
            row.update(state="settled", result=asdict(result), reconciliations=history,
                elapsed_s=time.time() - row["started_unix_s"], settled_unix_s=time.time())
            if call.provider == 'pi' and result.cost_usd is not None and money(result.cost_usd) > money(row['reserved_usd']):
                self.state['paid_blocked'] = 'actual paid cost exceeded original reservation'
            self.state["blocked"] = ""
            try:
                self._save()
            except OSError as exc:
                self.state["blocked"] = "original recovery persistence is unresolved"
                raise UnknownEffect(self.state["blocked"]) from exc

    def execute(self, call: OwnerCall) -> OwnerResult:
        with self.lock:
            if self.owner is None:
                raise Refused("original-owner bridge is not implemented")
            if self.state["blocked"]:
                raise UnknownEffect(self.state["blocked"])
            if call.role not in {"solver", "proposer", "selector", "reflection", "judge"}:
                raise Refused("owner role is unsupported")
            if not call.group or not call.run_id or not call.model:
                raise Refused("owner call binding is incomplete")
            if call.provider not in {"codex", "pi"}:
                raise Refused("owner route is unsupported")
            if call.provider == "pi" and call.model != "openrouter/deepseek/deepseek-v4.1-flash-1017":
                raise Refused("paid model differs from authorized route")
            call = OwnerCall(**json.loads(canonical(asdict(call))))
            key = json.dumps([call.group, call.run_id])
            digest = call.fingerprint()
            prior = self.state["calls"].get(key)
            if prior is not None:
                if prior["input_sha256"] != digest:
                    raise Refused("run id already binds different input")
                if prior["state"] != "settled":
                    raise Refused("original call has no settled reusable result")
                result = OwnerResult(**prior["result"])
                try:
                    self.verify_result(result, call)
                except UnknownEffect:
                    self.state["blocked"] = "original settled owner evidence changed"
                    self._save()
                    raise
                return replace(result, cached=True)
            reserve = None
            if call.provider == "pi":
                if self.state["paid_blocked"]:
                    raise Refused(self.state["paid_blocked"])
                remaining = Decimal("5") - self._spent()
                reserve = money(call.paid_reservation_usd) if call.paid_reservation_usd is not None else remaining
                if reserve <= 0 or self._spent() + reserve > Decimal("5"):
                    raise Refused("OpenRouter reservation exceeds remaining authorized budget")
            elif call.paid_reservation_usd is not None:
                raise Refused("subscription call has an unrelated paid reservation")
            row = {"input_sha256": digest, "call": asdict(call), "provider": call.provider, "role": call.role,
                   "state": "pending", "reserved_usd": str(reserve) if reserve is not None else None,
                   "started_unix_s": time.time(), "elapsed_s": None, "result": None}
            self.state["calls"][key] = row
            started = time.monotonic()
            try:
                self._save()  # intent before any owner effect
                result = self.owner.execute(call)
                self.verify_result(result, call)
                if result.billing_receipt is not None:
                    from openrouter_billing import verify_settlement
                    try:
                        self._require_unique_bill(key, verify_settlement(call, result, result.billing_receipt))
                    except Refused as exc:
                        raise UnknownEffect("new owner completion reuses another call's bill") from exc
                if call.fingerprint() != digest:
                    raise UnknownEffect("owner changed the frozen call")
                row.update(state="settled", result=asdict(result), elapsed_s=time.monotonic()-started, settled_unix_s=time.time())
                if reserve is not None and result.cost_usd is not None and money(result.cost_usd) > reserve:
                    self.state["paid_blocked"] = "owner paid cost exceeded its reservation"
                self._save()
                return result
            except Refused as exc:
                row.update(state="refused", error=str(exc), elapsed_s=time.monotonic()-started)
                self._save()
                raise
            except Exception as exc:
                row.update(state="unknown", error=type(exc).__name__)
                self.state["blocked"] = "original owner effect is unresolved"
                try:
                    self._save()
                except OSError as persistence_error:
                    raise UnknownEffect("owner effect unresolved; journal persistence failed") from persistence_error
                raise UnknownEffect(self.state["blocked"]) from exc
