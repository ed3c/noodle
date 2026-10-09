from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/codex-actions-capability.yml"


def block(lines, key, indent):
    header = " " * indent + key + ":"
    start = lines.index(header) + 1
    result = []
    for line in lines[start:]:
        if line.strip() and len(line) - len(line.lstrip()) <= indent:
            break
        result.append(line)
    return result


def runtime_selected(changed_paths, head_repository, repository):
    lines = WORKFLOW.read_text().splitlines()
    trigger = block(block(lines, "on", 0), "pull_request", 2)
    patterns = []
    for line in block(trigger, "paths", 4):
        if line.strip():
            if not line.startswith("      - "):
                raise ValueError("unsupported runtime path filter")
            patterns.append(line[8:].strip().strip("'\""))

    def matches(path, pattern):
        if pattern.endswith("/**"):
            prefix = pattern[:-2]
            return path.startswith(prefix) and len(path) > len(prefix)
        if any(char in pattern for char in "*?[]!"):
            raise ValueError("unsupported runtime path pattern: " + pattern)
        return path == pattern

    selected = any(matches(path, pattern) for path in changed_paths for pattern in patterns)
    job = block(block(lines, "jobs", 0), "worktree-runtime", 2)
    conditions = [line[8:] for line in job if line.startswith("    if: ")]
    if len(conditions) != 1:
        raise ValueError("runtime job requires one explicit repository condition")
    operands = conditions[0].split(" == ")
    values = {"github.event.pull_request.head.repo.full_name": head_repository,
              "github.repository": repository}
    if len(operands) != 2 or any(operand not in values for operand in operands):
        raise ValueError("unsupported runtime repository condition")
    return selected and values[operands[0]] == values[operands[1]]


class RuntimeCoverageTests(unittest.TestCase):
    def selected(self, paths, head_repository="ed3c/noodle"):
        return runtime_selected(paths, head_repository, "ed3c/noodle")

    def test_worktree_cli_change_requests_runtime_readback(self):
        self.assertTrue(self.selected(["cmd_worktree.go"]))

    def test_worktree_implementation_change_requests_runtime_readback(self):
        self.assertTrue(self.selected(["worktree/app.go"]))

    def test_go_module_change_requests_runtime_readback(self):
        self.assertTrue(self.selected(["go.mod"]))

    def test_go_checksum_change_requests_runtime_readback(self):
        self.assertTrue(self.selected(["go.sum"]))

    def test_existing_runtime_inputs_remain_selected(self):
        for path in ("dispatcher/process.go", ".github/workflows/codex-actions-capability.yml"):
            with self.subTest(path=path):
                self.assertTrue(self.selected([path]))

    def test_unrelated_changes_do_not_request_runtime_readback(self):
        for path in ("README.md", "docs/concepts.md", "ui/src/app.tsx", "cmd_review.go"):
            with self.subTest(path=path):
                self.assertFalse(self.selected([path]))

    def test_foreign_repository_cannot_run_selected_runtime_job(self):
        self.assertFalse(self.selected(["dispatcher/process.go"], "external/noodle"))


if __name__ == "__main__":
    unittest.main()
