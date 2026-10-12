#!/usr/bin/env python3
"""Exercise the release refresh against isolated local Git repositories."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("refresh-model-catalogs.sh")


class RefreshModelCatalogsTest(unittest.TestCase):
    def run_refresh(self, candidate, script=SCRIPT, valid_codex=True, raw_models=None):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            remote = root / "models"
            workspace = root / "workspace"
            bin_dir = root / "bin"
            for directory in (remote, workspace, bin_dir):
                directory.mkdir()
            def git(directory, *args):
                subprocess.run(["git", *args], cwd=directory, check=True,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            for directory in (remote, workspace):
                git(directory, "init", "-b", "main")
            (remote / "models.json").write_text(raw_models if raw_models is not None else json.dumps(candidate))
            (remote / "codex_client_models.json").write_text("valid" if valid_codex else "invalid")
            git(remote, "add", ".")
            git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "fixture")
            catalog_dir = workspace / "catalog"
            catalog_dir.mkdir()
            baseline = {"qodercn": [{"id": "qodercn/fallback", "context_length": 123}],
                        "qoder": [{"id": "qoder/fallback", "context_length": 456}],
                        "claude": [{"id": "old-claude"}]}
            (catalog_dir / "models.json").write_text(json.dumps(baseline))
            (catalog_dir / "codex_client_models.json").write_text("valid")
            go = bin_dir / "go"
            go.write_text('#!/usr/bin/env python3\nimport pathlib,sys\nsys.exit(0 if pathlib.Path(sys.argv[-1]).read_text()=="valid" else 1)\n')
            go.chmod(0o755)
            env = dict(os.environ, MODELS_REPOSITORY_URL=str(remote),
                       MODEL_CATALOG_DIR=str(catalog_dir), PATH=str(bin_dir)+os.pathsep+os.environ["PATH"])
            result = subprocess.run(["bash", str(script)], cwd=workspace, env=env,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            return result, json.loads((catalog_dir / "models.json").read_text()), (catalog_dir / "codex_client_models.json").read_text(), baseline

    def test_missing_or_empty_fork_sections_keep_baseline(self):
        for sections in ({}, {"qodercn": [], "qoder": []}, {"qodercn": None, "qoder": None}):
            with self.subTest(sections=sections):
                candidate = {"claude": [{"id": "new-claude"}], "unknown": [{"id": "new-provider"}], **sections}
                result, saved, codex, baseline = self.run_refresh(candidate)
                self.assertEqual(result.returncode, 0, result.stderr)
                for provider in ("qodercn", "qoder"):
                    self.assertEqual(saved.get(provider), baseline[provider])
                self.assertEqual(saved["claude"], candidate["claude"])
                self.assertEqual(saved["unknown"], candidate["unknown"])
                self.assertEqual(codex, "valid")

    def test_nonempty_remote_fork_sections_replace_baseline(self):
        candidate = {"qodercn": [{"id": "qodercn/live"}], "qoder": [{"id": "qoder/live"}]}
        result, saved, _, _ = self.run_refresh(candidate)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(saved, candidate)

    def test_invalid_fork_section_does_not_replace_catalog(self):
        for provider in ("qoder", "qodercn"):
            with self.subTest(provider=provider):
                result, saved, _, baseline = self.run_refresh({provider: {"invalid": True}})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(saved, baseline)

    def test_invalid_catalog_does_not_replace_baseline(self):
        for raw_models in ("{", "[]"):
            with self.subTest(raw_models=raw_models):
                result, saved, _, baseline = self.run_refresh({}, raw_models=raw_models)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(saved, baseline)

    def test_invalid_codex_keeps_existing_fallback(self):
        result, saved, codex, baseline = self.run_refresh({}, valid_codex=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(codex, "valid")
        self.assertEqual(saved["qoder"], baseline["qoder"])


if __name__ == "__main__":
    unittest.main()
