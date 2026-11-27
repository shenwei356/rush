import io
import json
import pathlib
import tarfile
import tempfile
import unittest

from scripts.update_scoop_manifest import ARCHIVES, update


class ScoopManifestTest(unittest.TestCase):
    def test_incomplete_release_preserves_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            work = pathlib.Path(directory)
            manifest = work / "manifest.json"
            original = json.dumps({"version": "1.0.0", "architecture": {arch: {} for arch in ARCHIVES}})
            manifest.write_text(original)
            for filename in ARCHIVES.values():
                with tarfile.open(work / filename, "w:gz") as archive:
                    member = tarfile.TarInfo("rush.exe")
                    member.size = 3
                    archive.addfile(member, io.BytesIO(b"exe"))
            last = work / ARCHIVES["arm64"]
            complete = last.read_bytes()
            last.write_bytes(complete[:-4])
            with self.assertRaises(EOFError):
                update("v1.0.1", work, manifest)
            self.assertEqual(manifest.read_text(), original)
            with tarfile.open(last, "w:gz") as archive:
                archive.addfile(tarfile.TarInfo("rush.exe"))
            with self.assertRaisesRegex(ValueError, "empty rush.exe"):
                update("v1.0.1", work, manifest)
            self.assertEqual(manifest.read_text(), original)
            last.write_bytes(complete)
            self.assertTrue(update("v1.0.1", work, manifest))
            self.assertEqual(json.loads(manifest.read_text())["version"], "1.0.1")
