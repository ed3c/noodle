"""Offline original-CLI transport controls; no Host or model invocation."""

import json
import shutil
import subprocess
import sys
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import test_soodles_bridge as fixtures
from composition_handoff import verify_derived_skills
from owner_contract import Refused, UnknownEffect, tree_ref
from soodles_bridge import PinnedCLI, decode, read_ref, reference, save


class CompositionHandoffTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.BridgeTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        asset = self.fixture.skill_base / "examples" / "public.html"
        asset.parent.mkdir()
        asset.write_text("<p>fixed example</p>")
        asset.chmod(0o751)
        (self.fixture.skill_base / "LICENSE").write_text("Fixture license")
        self.fixture.skill.write_text("Selected final native SKILL instructions.")
        self.call = replace(
            self.fixture.call,
            inputs={
                **self.fixture.call.inputs,
                "harness": tree_ref(self.fixture.harness),
            },
        )
        self.derived = self.root / "git-derived-source" / "main"
        shutil.copytree(self.fixture.skill_base, self.derived)
        (self.derived / "SKILL.md").write_bytes(self.fixture.skill.read_bytes())
        self.catalog = self.root / "fixed-catalog.json"
        self.catalog.write_text("{}")
        self.producer = self.root / "accepted-producer.json"
        self.producer.write_text("{}")
        self.artifact = self.root / "component.html"
        self.artifact.write_text("<p>accepted A</p>")
        self.template = replace(
            self.fixture.template,
            skill_bases={"main": tree_ref(self.fixture.skill_base)},
            composition={
                "definition": reference(self.catalog),
                "id": "fixture-consumer",
            },
            producer_handoff={
                "request": reference(self.producer),
                "artifact_path": "component.html",
            },
        )
        script = self.root / "original-owner-fixture.py"
        script.write_text("# Explicit transport fixture, not Soodles.\n")
        self.cli = PinnedCLI(
            reference(Path(sys.executable).resolve()),
            reference(script),
            (reference(script),),
            "a" * 40,
            {},
            10,
        )
        self.calls = []
        self.mutate = lambda request, receipt: None

    def transport(self, argv, **kwargs):
        self.calls.append(argv)
        self.assertEqual(argv[3:5], ["loop", "handoff"])

        def value(option):
            return argv[argv.index(option) + 1]

        raw_ref = reference(Path(value("--consumer-template")))
        raw = decode(read_ref(raw_ref))
        self.assertFalse(
            {"runtime", "provider", "model", "selected_skill", "composition"}
            & raw.keys()
        )
        output = Path(value("--output"))
        handoff = output.with_name(output.name + ".handoff.json")
        receipt = {
            "schema": 1,
            "owner": "soodles.loop",
            "effects": [],
            "authorizes_landing": False,
            "consumer_composition": self.template.composition,
            "consumer_template": raw_ref,
            "consumer_request_path": str(output),
            "producer": {
                "request": reference(self.producer),
                "artifact_path": "component.html",
                "artifact": reference(self.artifact),
                "candidate": reference(self.producer),
                "task_envelope": reference(self.producer),
                "acceptance": reference(self.producer),
            },
        }
        request = {
            **raw,
            "runtime": "openshell",
            "provider": self.call.provider,
            "model": self.call.model,
            "model_reasoning_effort": "low",
            "profile": self.template.profile["path"],
            "selected_skill": str(self.derived / "SKILL.md"),
            "support_skills": [],
            "composition": {**self.template.composition, "stage_id": "render"},
            "skill_bindings": [
                {
                    "source": "derived",
                    "alias": "main",
                    "path": str(self.derived / "SKILL.md"),
                }
            ],
        }
        self.mutate(request, receipt)
        receipt_ref = save(handoff, receipt)
        request.update(
            handoff=receipt_ref,
            source_refs=list(
                dict.fromkeys(raw["source_refs"] + [str(self.artifact), str(handoff)])
            ),
        )
        result = {
            "owner": "soodles.loop",
            "status": "prepared",
            "effects": [],
            "authorizes_landing": False,
            "request": save(output, request),
            "handoff": receipt_ref,
        }
        kwargs["stdout"].write(json.dumps(result).encode())
        return subprocess.CompletedProcess(argv, 0)

    def prepare(self, name="call"):
        directory = self.root / name
        directory.mkdir()
        with patch("composition_handoff.subprocess.run", side_effect=self.transport):
            result = self.template.prepare(self.call, directory, cli=self.cli)
        return directory, result

    def test_fixed_owner_handoff_retains_complete_assets_and_never_enters(self):
        directory, ref = self.prepare()
        request = decode(read_ref(ref))
        self.assertEqual(len(self.calls), 1)
        self.assertIn(str(self.artifact), request["source_refs"])
        self.assertEqual(
            request["composition"], {**self.template.composition, "stage_id": "render"}
        )
        self.assertEqual(
            decode((directory / "instruction-trees.json").read_bytes()),
            [tree_ref(self.derived)],
        )
        self.assertEqual(
            (self.derived / "examples/public.html").stat().st_mode & 0o777, 0o751
        )
        verify_derived_skills(self.template, self.call, request)

    def test_changed_or_added_or_deleted_fixed_asset_refuses(self):
        request = {
            "selected_skill": str(self.derived / "SKILL.md"),
            "support_skills": [],
            "skill_bindings": [
                {"alias": "main", "path": str(self.derived / "SKILL.md")}
            ],
        }
        asset = self.derived / "examples/public.html"
        raw = asset.read_bytes()
        for case in ("changed", "added", "deleted", "mode", "skill"):
            with self.subTest(case=case):
                extra = self.derived / "foreign.txt"
                if case == "changed":
                    asset.write_text("other")
                if case == "added":
                    extra.write_text("other")
                if case == "deleted":
                    asset.unlink()
                if case == "mode":
                    asset.chmod(0o644)
                if case == "skill":
                    (self.derived / "SKILL.md").write_text("unselected proposal")
                with self.assertRaises(Refused):
                    verify_derived_skills(self.template, self.call, request)
                asset.write_bytes(raw)
                asset.chmod(0o751)
                extra.unlink(missing_ok=True)
                (self.derived / "SKILL.md").write_bytes(self.fixture.skill.read_bytes())
        verify_derived_skills(self.template, self.call, request)

    def test_foreign_producer_route_or_selection_preserves_unknown_without_enter(self):
        original = self.template.composition
        for field in ("producer", "profile", "model", "selection"):

            def mutate(request, receipt):
                if field == "producer":
                    receipt["producer"]["request"] = reference(self.artifact)
                if field == "profile":
                    request["profile"] = "/foreign-profile"
                if field == "model":
                    request["model"] = "foreign-model"
                if field == "selection":
                    request["composition"]["id"] = "other"

            self.mutate = mutate
            with self.subTest(field=field), self.assertRaises(UnknownEffect):
                self.prepare(field)
            self.assertTrue(
                (self.root / field / "handoff-preparation/unknown.json").is_file()
            )
        self.assertEqual(self.template.composition, original)
        self.assertTrue(all(argv[4] == "handoff" for argv in self.calls))

    def test_missing_pair_refuses_and_transport_loss_is_not_replayed(self):
        incomplete = replace(self.template, producer_handoff=None)
        directory = self.root / "incomplete"
        directory.mkdir()
        with self.assertRaises(Refused):
            incomplete.prepare(self.call, directory, cli=self.cli)
        self.assertEqual(list(directory.iterdir()), [])
        directory = self.root / "lost"
        directory.mkdir()
        with patch(
            "composition_handoff.subprocess.run",
            side_effect=TimeoutError("fixture lost reply"),
        ) as run:
            with self.assertRaises(UnknownEffect):
                self.template.prepare(self.call, directory, cli=self.cli)
        self.assertEqual(run.call_count, 1)
        self.assertFalse((directory / "operation-0000").exists())
        self.assertFalse((directory / "request.json").exists())
