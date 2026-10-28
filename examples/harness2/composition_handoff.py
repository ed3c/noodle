"""Fixed heldout consumers use the original Soodles accepted-producer handoff."""

import hashlib
import subprocess
from pathlib import Path

from owner_contract import UnknownEffect, canonical, tree_ref
from soodles_bridge import decode, read_ref, reference, require, save

FIXED = {
    "runtime",
    "provider",
    "model",
    "model_reasoning_effort",
    "profile",
    "selected_skill",
    "support_skills",
}


def verify_derived_skills(template, call, request):
    """Only selected SKILL bytes may vary; fixed assets, inventory and modes cannot."""
    harness = call.inputs["harness"]
    require(tree_ref(harness["path"]) == harness, "selected native harness changed")
    paths = [request.get("selected_skill"), *request.get("support_skills", [])]
    bindings = request.get("skill_bindings") or []
    require(
        len(paths) == len(bindings) == len(template.harness_skills),
        "derived skill inventory differs",
    )
    for alias, path, binding in zip(template.harness_skills, paths, bindings):
        require(
            binding.get("alias") == alias
            and binding.get("path") == path
            and isinstance(path, str)
            and Path(path).name == "SKILL.md",
            "derived skill binding differs",
        )
        base = template.skill_bases[alias]
        require(tree_ref(base["path"]) == base, "fixed skill assets changed")
        candidate = reference(Path(harness["path"]) / "skills" / alias / "SKILL.md")
        inventory = []
        for source in sorted(Path(base["path"]).rglob("*")):
            if not source.is_file():
                continue
            name = source.relative_to(base["path"]).as_posix()
            inventory.append(
                [
                    name,
                    source.stat().st_mode & 0o777,
                    candidate["sha256"]
                    if name == "SKILL.md"
                    else reference(source)["sha256"],
                ]
            )
        require(
            any(row[0] == "SKILL.md" for row in inventory),
            "fixed skill entry is absent",
        )
        expected = hashlib.sha256(canonical(inventory)).hexdigest()
        require(
            tree_ref(Path(path).parent)["sha256"] == expected,
            "derived skill changed fixed asset inventory, bytes or modes",
        )
        require(
            tree_ref(base["path"]) == base and tree_ref(harness["path"]) == harness,
            "evaluated source changed during derived skill verification",
        )


def verify_composed_request(template, expected, actual, receipt, receipt_ref):
    """Independent check of original owner output and the full evaluated skill tree."""
    selection, upstream = template.composition, template.producer_handoff
    require(
        receipt.get("owner") == "soodles.loop"
        and receipt.get("schema") == 1
        and receipt.get("effects") == []
        and receipt.get("authorizes_landing") is False
        and receipt.get("consumer_composition") == selection
        and "composition" not in receipt,
        "original independent composition receipt differs",
    )
    producer = receipt.get("producer") or {}
    require(
        producer.get("request") == upstream["request"]
        and producer.get("artifact_path") == upstream["artifact_path"],
        "original accepted producer binding differs",
    )
    for key in ("request", "artifact", "candidate", "task_envelope", "acceptance"):
        read_ref(producer[key])
    require(
        actual.get("handoff") == receipt_ref, "original handoff receipt is detached"
    )
    composition = actual.get("composition") or {}
    require(
        set(composition) == {"definition", "id", "stage_id"}
        and {key: composition[key] for key in ("definition", "id")} == selection
        and isinstance(composition["stage_id"], str)
        and composition["stage_id"],
        "original consumer composition selection differs",
    )
    extra = {
        "composition",
        "skill_bindings",
        "handoff",
        "selected_skill",
        "support_skills",
        "source_refs",
    }
    require(
        {key: value for key, value in actual.items() if key not in extra}
        == {key: value for key, value in expected.items() if key not in extra},
        "original consumer changed fixed task or execution environment",
    )
    sources = list(
        dict.fromkeys(
            expected["source_refs"]
            + [producer["artifact"]["path"], receipt_ref["path"]]
        )
    )
    require(
        actual.get("source_refs") == sources, "original consumer source inputs differ"
    )
    paths = [actual.get("selected_skill"), *actual.get("support_skills", [])]
    expected_paths = [expected["selected_skill"], *expected["support_skills"]]
    rows = actual.get("skill_bindings") or []
    require(
        len(paths) == len(expected_paths) == len(rows) == len(template.harness_skills),
        "original qualified skill inventory differs",
    )
    trees = []
    for path, original, row, alias in zip(
        paths, expected_paths, rows, template.harness_skills
    ):
        require(
            isinstance(path, str)
            and Path(path).name == "SKILL.md"
            and row.get("path") == path
            and row.get("alias") == alias,
            "original qualified skill alias or entry differs",
        )
        current, evaluated = (
            tree_ref(Path(path).parent),
            tree_ref(Path(original).parent),
        )
        require(
            current["sha256"] == evaluated["sha256"],
            "derived skill differs from evaluated SKILL.md and complete fixed assets",
        )
        trees.append(current)
    return trees


def prepare_composition(cli, template, call, directory, expected):
    """Prepare local files through the pinned original CLI; never enter a task."""
    require(
        cli is not None and template.role == "solver",
        "fixed composition needs original solver CLI",
    )
    selected, producer = template.composition, template.producer_handoff
    require(
        isinstance(selected, dict) and set(selected) == {"definition", "id"},
        "fixed composition selection is malformed",
    )
    require(
        isinstance(producer, dict) and set(producer) == {"request", "artifact_path"},
        "original accepted producer selection is malformed",
    )
    for ref in (selected["definition"], producer["request"]):
        read_ref(ref)
    cli.verify()
    raw = {key: value for key, value in expected.items() if key not in FIXED}
    raw_ref = save(directory / "consumer-template.json", raw)
    selection_ref = save(directory / "consumer-composition.json", selected)
    evidence = directory / "handoff-preparation"
    evidence.mkdir()
    output = directory / "request.json"
    argv = [
        cli.python["path"],
        "-B",
        cli.entrypoint["path"],
        "loop",
        "handoff",
        "--producer-request",
        producer["request"]["path"],
        "--artifact",
        producer["artifact_path"],
        "--consumer-template",
        raw_ref["path"],
        "--consumer-composition",
        selection_ref["path"],
        "--output",
        str(output),
    ]
    save(
        evidence / "intent.json",
        {
            "source_commit": cli.source_commit,
            "python": cli.python,
            "sources": cli.sources,
            "producer": producer,
            "template": raw_ref,
            "composition": selected,
            "argv": argv,
        },
    )
    try:
        with (
            (evidence / "stdout.json").open("xb") as out,
            (evidence / "stderr.log").open("xb") as err,
        ):
            process = subprocess.run(
                argv,
                cwd=Path(cli.entrypoint["path"]).parent,
                env={**cli.environment, "PYTHONDONTWRITEBYTECODE": "1"},
                stdout=out,
                stderr=err,
                timeout=cli.timeout_s,
                check=False,
            )
        save(
            evidence / "receipt.json",
            {
                "exit_code": process.returncode,
                "stdout": reference(evidence / "stdout.json"),
                "stderr": reference(evidence / "stderr.log"),
            },
        )
        cli.verify()
        for ref in (
            raw_ref,
            selection_ref,
            selected["definition"],
            producer["request"],
        ):
            read_ref(ref)
        result = decode(read_ref(reference(evidence / "stdout.json")))
        require(
            process.returncode == 0
            and result.get("owner") == "soodles.loop"
            and result.get("status") == "prepared"
            and result.get("effects") == []
            and result.get("authorizes_landing") is False,
            "original composition handoff preparation refused or unresolved",
        )
        request_ref = reference(output)
        require(
            result.get("request") == request_ref,
            "original prepared request identity differs",
        )
        receipt_ref = result["handoff"]
        receipt = decode(read_ref(receipt_ref))
        require(
            receipt.get("consumer_template") == raw_ref
            and receipt.get("consumer_request_path") == str(output),
            "original consumer template or destination differs",
        )
        actual = decode(read_ref(request_ref))
        trees = verify_composed_request(
            template, expected, actual, receipt, receipt_ref
        )
        verify_derived_skills(template, call, actual)
        save(
            directory / "composition-preparation.json",
            {
                "request": request_ref,
                "expected": expected,
                "handoff": receipt_ref,
                "owner_readback": reference(evidence / "stdout.json"),
                "skill_trees": trees,
            },
        )
        return request_ref, trees
    except Exception as error:
        save(
            evidence / "unknown.json",
            {
                "error_type": type(error).__name__,
                "operation": "original_handoff_prepare",
                "native_enter_started": False,
                "partial_files_preserved": True,
            },
        )
        raise UnknownEffect(
            "original handoff preparation unresolved; inspect saved files, never replay"
        ) from error
