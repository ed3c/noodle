import json
import tempfile
import unittest
from pathlib import Path

import reproduce


class ReproducerTests(unittest.TestCase):
    def test_archive_patch_and_license_are_fixed(self):
        self.assertEqual(reproduce.digest(reproduce.HERE / "noodle-owner.patch"),
                         reproduce.PIN["patch_sha256"])
        self.assertEqual(reproduce.digest(reproduce.HERE / "LICENSE.upstream"),
                         reproduce.PIN["license_sha256"])
        self.assertEqual(reproduce.PIN["commit"], "1cfe254e8d25b15f37048c3730134686abe60335")

    def test_bad_archive_refuses_before_source_extract_or_patch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "bad.tar.gz"
            archive.write_bytes(b"not upstream")
            with self.assertRaisesRegex(ValueError, "archive differs"):
                reproduce.prepare(root / "output", archive)
            self.assertEqual([p.name for p in (root / "output").iterdir()], ["source.tar.gz"])

    def test_prepared_source_drift_refuses_before_test_process(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / ("harness2-" + reproduce.PIN["commit"])
            source.mkdir()
            (source / "source.py").write_text("original")
            (root / "prepared.json").write_text(json.dumps({"source_pin": reproduce.PIN,
                "source_tree_sha256": reproduce.fingerprint(source)}))
            (source / "source.py").write_text("changed")
            with self.assertRaisesRegex(ValueError, "drifted"):
                reproduce.controls(root)
