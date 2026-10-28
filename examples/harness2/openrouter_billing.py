"""Original OpenRouter GET-only settlement; no token pricing or model transport."""
import hashlib
import json
import re
import shlex
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path

from owner_contract import Refused, money, tree_ref
from soodles_bridge import MODELS, decode, read_ref, require, save, unique

BILLED_MODEL = "deepseek/deepseek-v4.1-flash-20261017"


class BillingUnavailable(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        raise BillingUnavailable("billing endpoint redirected")


class ReadOnlyOpenRouter:
    """Private supplier: only its process consumes the explicit credential locator.

    Never pass this supplier or its environment file to a model/context bundle.
    """
    def __init__(self, environment_file: Path, *, key_name="OPENROUTER_API_KEY", timeout_s=30):
        self.environment_file, self.key_name, self.timeout_s = Path(environment_file), key_name, timeout_s
        self._token = None
        self.key_fingerprint = None

    def _credential(self):
        if self._token is None:
            path = self.environment_file
            require(path.is_absolute() and path.resolve() == path and path.is_file(),
                    "explicit billing credential file is unavailable")
            values = []
            for line in path.read_text().splitlines():
                key, separator, value = line.removeprefix("export ").partition("=")
                if separator and key.strip() == self.key_name:
                    parts = shlex.split(value, comments=True)
                    require(len(parts) == 1, "billing credential assignment is invalid")
                    values.append(parts[0])
            require(len(values) == 1 and re.fullmatch(r"sk-or-[A-Za-z0-9_-]+", values[0]),
                    "billing credential is absent, duplicated or invalid")
            self._token = values[0]
            self.key_fingerprint = hashlib.sha256(self._token.encode()).hexdigest()
        return self._token

    def get(self, resource, *, generation_id=None):
        require(resource in {"key", "generation"}, "billing endpoint is outside GET allowlist")
        require((resource == "key" and generation_id is None) or
                (resource == "generation" and re.fullmatch(r"gen-[A-Za-z0-9-]+", generation_id or "")),
                "billing generation identity is invalid")
        url = "https://openrouter.ai/api/v1/" + resource
        if generation_id is not None:
            url += "?" + urllib.parse.urlencode({"id": generation_id})
        token = self._credential()
        request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token}, method="GET")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        try:
            with opener.open(request, timeout=self.timeout_s) as response:
                raw = response.read(2 * 1024 * 1024 + 1)
            require(len(raw) <= 2 * 1024 * 1024, "billing response exceeds metadata bound")
            value = json.loads(raw, object_pairs_hook=unique, parse_float=Decimal, parse_int=Decimal,
                               parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite billing")))
            data = value["data"]
            require(isinstance(data, dict), "billing response has no data object")
            # Save only metadata, never credentials, headers, prompts or key labels.
            fields = (("limit", "limit_remaining", "limit_reset", "usage", "is_free_tier", "is_management_key")
                      if resource == "key" else
                      ("id", "model", "total_cost", "usage", "finish_reason", "cancelled", "is_byok", "created_at"))
            selected = {key: str(data[key]) if isinstance(data.get(key), Decimal) else data.get(key) for key in fields}
            return {"resource": resource, "generation_id": generation_id, "data": selected,
                    "response_sha256": hashlib.sha256(raw).hexdigest(), "key_fingerprint": self.key_fingerprint,
                    "observed_at": datetime.now(timezone.utc).isoformat()}
        except urllib.error.HTTPError as error:
            raise BillingUnavailable("billing GET returned HTTP " + str(error.code)) from None
        except (urllib.error.URLError, TimeoutError, ValueError, KeyError, Refused) as error:
            raise BillingUnavailable("billing GET metadata unavailable: " + type(error).__name__) from None


def key_usage(value, fingerprint=None):
    require(value.get("resource") == "key", "key readback has a different endpoint")
    require(re.fullmatch(r"[0-9a-f]{64}", value.get("key_fingerprint", "")), "key identity is unavailable")
    if fingerprint is not None:
        require(value["key_fingerprint"] == fingerprint, "original billing key changed")
    data = value["data"]
    require(data.get("limit_reset") is None and data.get("is_management_key") is not True,
            "billing key has a reset interval or management role")
    limit, usage, remaining = (money(data[key]) for key in ("limit", "usage", "limit_remaining"))
    require(limit <= Decimal("5") and limit > 0 and usage <= limit and remaining == limit - usage,
            "original key cap or remaining usage is inconsistent")
    return usage, remaining


def retained(result, ref):
    directory = Path(result.run_dir)
    require(tree_ref(directory)["sha256"] == result.output_sha256, "original result bytes changed")
    rows = decode((directory / "evidence/index.json").read_bytes())
    matches = [row["saved"] for row in rows if row["original"] == ref]
    require(matches and all(row == matches[0] for row in matches), "original capture has no unique retained file")
    require(Path(matches[0]["path"]).is_relative_to(directory), "retained billing source is outside original result")
    read_ref(matches[0])
    return matches[0]


def generation_ids(result):
    evidence = decode((Path(result.run_dir) / "native-evidence.json").read_bytes())
    descriptor_ref = retained(result, evidence["descriptor"])
    descriptor = decode(read_ref(descriptor_ref))
    require(descriptor["provider"] == "pi" and not descriptor.get("gaps") and descriptor.get("terminal"),
            "original Pi capture is incomplete")
    raw_ref = retained(result, descriptor["raw"])
    return pi_generation_ids(read_ref(raw_ref)), descriptor_ref, raw_ref


def pi_generation_ids(raw):
    ids = []
    for line in raw.splitlines():
        row = decode(line)
        message = row.get("message", {})
        if row.get("type") != "message" or message.get("role") != "assistant":
            continue
        require(message.get("provider") == "openrouter" and
                message.get("model") == MODELS["pi"].removeprefix("openrouter/"),
                "native billed message belongs to another model")
        identity = message.get("responseId")
        require(isinstance(identity, str) and re.fullmatch(r"gen-[A-Za-z0-9-]+", identity),
                "native assistant message has no original generation identity")
        require(identity not in ids, "native generation identity is duplicated")
        ids.append(identity)
    require(ids, "original native capture has no billable generations")
    return ids


class OpenRouterBilling:
    """One serial dedicated-key owner; external writers are not locked by Python.

    The operator declares exclusive use. Every settlement also reconciles actual
    generation bills against the key delta, so observed interference keeps holds.
    """
    def __init__(self, client, *, initial_usage_usd: str, exclusive_key: bool,
                 observations=3, interval_s=1.0):
        require(exclusive_key is True, "original key has no exclusive owner declaration")
        require(type(observations) is int and observations > 0 and interval_s >= 0,
                "billing observation bound is invalid")
        self.client, self.expected_usage = client, money(initial_usage_usd)
        self.observations, self.interval_s = observations, interval_s
        self.pending = None

    def begin(self, call, directory):
        require(call.provider == "pi" and call.model == MODELS["pi"], "billing call route differs")
        require(self.pending is None, "original paid call still has an unresolved bill")
        value = self.client.get("key")
        usage, remaining = key_usage(value)
        require(usage == self.expected_usage and remaining > 0, "key usage changed outside the original serial owner")
        directory = Path(directory)
        directory.mkdir()
        reference = save(directory / "before.json", {"schema_version": 1, "call_sha256": call.fingerprint(),
                         "exclusive_key": True, "key": value})
        self.pending = call.fingerprint()
        return reference

    def settle(self, call, before_ref, result, directory):
        """Bounded GET observations only; re-call for the SAME call after an unknown bill."""
        require(result.settled, "original native call is not settled")
        directory = Path(directory)
        directory.mkdir()  # new evidence for each explicit read-only reconciliation
        before = decode(read_ref(before_ref))
        require(before.get("call_sha256") == call.fingerprint() and before.get("exclusive_key") is True,
                "billing interval belongs to another call")
        require(self.pending in {None, call.fingerprint()}, "another original paid interval is unresolved")
        self.pending = call.fingerprint()
        usage_before, _ = key_usage(before["key"])
        last_reason = "billing_not_observed"
        ids, descriptor, raw = [], None, None
        for index in range(self.observations):
            try:
                ids, descriptor, raw = generation_ids(result)
                bills, total = [], Decimal("0")
                for generation_index, identity in enumerate(ids):
                    bill = self.client.get("generation", generation_id=identity)
                    save(directory / f"generation-{index:03d}-{generation_index:03d}.json", bill)
                    data = bill["data"]
                    require(bill.get("key_fingerprint") == before["key"]["key_fingerprint"]
                            and data.get("id") == identity and data.get("model") == BILLED_MODEL
                            and data.get("is_byok") is False
                            and (bool(data.get("finish_reason")) or data.get("cancelled") is True),
                            "original generation bill is incomplete or belongs to another route")
                    total += money(data["total_cost"])
                    bills.append(bill)
                after = self.client.get("key")
                save(directory / f"key-after-{index:03d}.json", after)
                usage_after, _ = key_usage(after, before["key"]["key_fingerprint"])
                sample = {"generations": bills, "key_after": after, "generation_total_usd": str(total)}
                sample_ref = save(directory / f"observation-{index:03d}.json", sample)
                require(usage_after >= usage_before and usage_after - usage_before == total,
                        "generation bills and exclusive key delta differ; delayed billing or other consumption")
                receipt = {"schema_version": 1, "status": "settled", "call_sha256": call.fingerprint(),
                    "result_sha256": result.output_sha256, "cost_usd": str(total), "generation_ids": ids,
                    "before": before_ref, "observation": sample_ref, "descriptor": descriptor, "raw": raw,
                    "key_usage_after_usd": str(usage_after), "model_calls": 0, "provider_mutations": 0}
                ref = save(directory / "settlement.json", receipt)
                self.expected_usage, self.pending = usage_after, None
                return ref
            except (BillingUnavailable, Refused, KeyError, ValueError) as error:
                last_reason = str(error) if isinstance(error, (BillingUnavailable, Refused)) else type(error).__name__
                save(directory / f"unknown-{index:03d}.json", {"status": "unknown", "reason": last_reason})
                if index + 1 < self.observations:
                    time.sleep(self.interval_s)
        return save(directory / "settlement.json", {"schema_version": 1, "status": "unknown",
            "call_sha256": call.fingerprint(), "result_sha256": result.output_sha256,
            "cost_usd": None, "generation_ids": ids, "before": before_ref,
            "reason": last_reason, "model_calls": 0, "provider_mutations": 0})


def verify_settlement(call, result, ref):
    require(result.settled, "original native call is not settled")
    receipt = decode(read_ref(ref))
    require(receipt.get("schema_version") == 1 and receipt.get("status") == "settled"
            and receipt.get("result_sha256") == result.output_sha256,
            "billing receipt is unresolved or belongs to another result")
    if call is not None:
        require(call.provider == "pi" and receipt.get("call_sha256") == call.fingerprint(),
                "billing receipt belongs to another call")
    before = decode(read_ref(receipt["before"]))
    sample = decode(read_ref(receipt["observation"]))
    require(before.get("call_sha256") == receipt["call_sha256"] and before.get("exclusive_key") is True,
            "billing interval identity differs")
    old, _ = key_usage(before["key"])
    new, _ = key_usage(sample["key_after"], before["key"]["key_fingerprint"])
    ids, descriptor, raw = generation_ids(result)
    require(receipt.get("generation_ids") == ids and receipt.get("descriptor") == descriptor
            and receipt.get("raw") == raw and len(sample["generations"]) == len(ids),
            "settled generation set differs from original native trace")
    total = Decimal("0")
    for identity, bill in zip(ids, sample["generations"]):
        data = bill["data"]
        require(bill.get("key_fingerprint") == before["key"]["key_fingerprint"]
                and data.get("id") == identity and data.get("model") == BILLED_MODEL
                and data.get("is_byok") is False
                and (data.get("finish_reason") or data.get("cancelled") is True),
                "settled original generation differs")
        total += money(data["total_cost"])
    require(new - old == total == money(receipt["cost_usd"])
            and money(receipt["key_usage_after_usd"]) == new,
            "settled actual bill differs from original key delta")
    return receipt
