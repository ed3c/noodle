"""Fixed, local training-only drawing feedback; never an owner or score adapter.

group['training_feedback'] explicitly supplies scope='training', node (FileRef),
engine (all three sibling drawing-*.mjs FileRefs), timeout_s, and allowlist:
[{task_id, source: TreeRef, source_files: [FileRef], source_facts: FileRef}].
Source facts are independent of editable instructions. No task is auto-enrolled.
"""
import hashlib
import math
import os
import subprocess
import threading
from dataclasses import asdict
from pathlib import Path

from owner_contract import Refused, canonical, tree_ref
from soodles_bridge import decode, read_ref, reference, require, save

ENGINE_NAMES = ('drawing-quality.mjs', 'drawing-visual.mjs', 'drawing-facts.mjs')
EXIT_STATUS = {'PASS': 0, 'FAIL': 1, 'UNKNOWN': 2}


def aggregate(statuses):
    require(statuses and all(s in EXIT_STATUS for s in statuses), 'quality statuses are invalid')
    return 'FAIL' if 'FAIL' in statuses else 'UNKNOWN' if 'UNKNOWN' in statuses else 'PASS'


def disjoint(left, right):
    return not (left.is_relative_to(right) or right.is_relative_to(left))


def public_finding(category, finding):
    """Bounded locations from the submitted artifact, never oracle expectations."""
    result = {'category': category, 'status': finding['status'],
              'kind': str(finding.get('kind', 'unspecified'))[:120]}
    for key, limit in (('code', 64), ('location', 200)):
        if isinstance(finding.get(key), str):
            result[key] = finding[key][:limit]
    def text_location(value):
        if isinstance(value, str):
            return value[:160]
        if isinstance(value, dict) and isinstance(value.get('text'), str):
            item = {'text': value['text'][:160]}
            box = value.get('box')
            if isinstance(box, dict) and set(box) == {'x', 'y', 'width', 'height'} and all(
                    type(v) in (int, float) and math.isfinite(v) for v in box.values()):
                item['box'] = box
            return item
        return None
    if 'text' in finding:
        result['text'] = text_location(finding['text'])
    if isinstance(finding.get('texts'), list):
        result['texts'] = [text_location(t) for t in finding['texts'][:2]]
    if type(finding.get('pixels')) is int and finding['pixels'] >= 0:
        result['pixels'] = finding['pixels']
    return result


class DrawingFeedback:
    def __init__(self, config, output):
        # Freeze caller-owned dictionaries before any proposer can run.
        self.config = decode(canonical(config))
        require(set(self.config) == {'scope', 'node', 'engine', 'timeout_s', 'allowlist'}
                and self.config['scope'] == 'training', 'quality feedback scope is not explicit training')
        timeout = self.config['timeout_s']
        require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0,
                'quality checker timeout is invalid')
        engine = self.config['engine']
        expected = [str(Path(__file__).resolve().parent / name) for name in ENGINE_NAMES]
        require(isinstance(engine, list) and [r.get('path') for r in engine] == expected,
                'quality engine closure differs from the fixed checker')
        self.output = Path(output).absolute()
        require(self.output.resolve() == self.output, 'quality output traverses a link')
        rows = self.config['allowlist']
        require(isinstance(rows, list) and rows, 'training quality allowlist is empty')
        self.tasks = {}
        for row in rows:
            require(isinstance(row, dict) and set(row) == {'task_id', 'source', 'source_files', 'source_facts'}
                    and isinstance(row['task_id'], str) and row['task_id'] not in self.tasks,
                    'training quality task binding is invalid or duplicated')
            self.tasks[row['task_id']] = row
        self._lock = threading.RLock()
        self.verify()
        self.check_ready()

    def check_ready(self):
        with self._lock:
            require(self.output.resolve() == self.output, 'quality output traverses a link')
            if self.output.exists():
                for directory in self.output.iterdir():
                    record_path = directory / 'feedback.json'
                    require(record_path.is_file(), 'quality checker execution is unresolved; retry is blocked')
                    record = decode(read_ref(reference(record_path)))
                    require(record['execution_status'] == 'completed'
                            and record['binding']['checker'] == self.config,
                            'quality checker execution is unknown or changed; owner dispatch is blocked')

    def verify(self):
        require(self.output.resolve() == self.output, 'quality output traverses a link')
        for ref in [self.config['node'], *self.config['engine']]:
            read_ref(ref)
            require(disjoint(self.output, Path(ref['path'])), 'quality output overlaps checker')
        require(os.access(self.config['node']['path'], os.X_OK), 'frozen Node is not executable')
        for row in self.tasks.values():
            root = Path(row['source']['path'])
            require(tree_ref(root) == row['source'], 'training quality source changed')
            require(disjoint(self.output, root), 'quality output overlaps training source')
            refs = row['source_files']
            require(isinstance(refs, list) and refs and len({r['path'] for r in refs}) == len(refs),
                    'training source FileRefs are missing or duplicated')
            for ref in refs:
                read_ref(ref)
                require(Path(ref['path']).is_relative_to(root), 'training source file belongs to another task')
            facts = row['source_facts']
            require(isinstance(decode(read_ref(facts)), dict), 'training source facts are not an object')
            require(disjoint(self.output, Path(facts['path'])), 'quality output overlaps source facts')

    def bind_tasks(self, sources, baseline, native_output):
        require(all(sources.get(key) == row['source'] for key, row in self.tasks.items()),
                'training quality allowlist differs from adapter tasks')
        require(disjoint(self.output, Path(baseline['path'])) and disjoint(self.output, native_output),
                'quality output overlaps native output or editable harness')
        for row in self.tasks.values():
            require(disjoint(Path(row['source_facts']['path']), Path(baseline['path']))
                    and disjoint(Path(row['source_facts']['path']), native_output),
                    'source facts overlap editable instructions')

    def __call__(self, result, call, deliverables, gate):
        row = self.tasks.get(call.inputs['task_id'])
        if row is None:
            return None
        with self._lock:
            gate.verify_result(result, call)
            self.verify()
            require(call.role == 'solver' and call.inputs['source'] == row['source'],
                    'quality feedback call differs from training source')
            require(disjoint(Path(row['source_facts']['path']), Path(call.inputs['harness']['path'])),
                    'source facts overlap evaluated instructions')
            run = Path(result.run_dir)
            require(disjoint(self.output, run), 'quality output overlaps original result')
            # snapshot_candidate retains these exact bytes, even after owner worktree cleanup.
            html = reference(run / 'workspace' / 'drawing.html')
            raw = read_ref(html)
            require(isinstance(deliverables.get('drawing.html'), str)
                    and deliverables['drawing.html'].encode('utf-8') == raw,
                    'drawing projection differs from original workspace bytes')
            binding = {'scope': 'training', 'call': asdict(call), 'call_sha256': call.fingerprint(),
                       'result': asdict(result), 'artifact': html, 'training_task': row,
                       'checker': self.config}
            # cached is transport metadata; it does not change the original run identity.
            binding['result'].pop('cached')
            key = hashlib.sha256(canonical(binding)).hexdigest()
            directory = self.output / key
            if directory.exists():
                record = decode((directory / 'feedback.json').read_bytes()) if (directory / 'feedback.json').is_file() else None
                require(record is not None and record['binding'] == binding,
                        'quality checker execution is unresolved; retry is blocked')
                require(tree_ref(directory / 'execution') == record['execution'], 'quality execution evidence changed')
                require(record['execution_status'] == 'completed', 'quality checker execution is unknown; retry is blocked')
                event = self._report(record['report'], directory / 'execution' / 'checker', html, row, binding)
                require(event == record['event'], 'quality feedback projection changed')
                return event
            directory.mkdir(parents=True, exist_ok=False)
            save(directory / 'intent.json', binding)
            execution = directory / 'execution'
            execution.mkdir()
            report_ref = None
            try:
                # Empty provider/Host credentials and no inherited NODE_OPTIONS.
                environment = {'PATH': os.defpath, 'HOME': str(Path.home()), 'TMPDIR': str(execution)}
                with (execution / 'stdout').open('xb') as stdout, (execution / 'stderr').open('xb') as stderr:
                    completed = subprocess.run([self.config['node']['path'], self.config['engine'][0]['path'],
                        html['path'], str(execution / 'checker'), row['source_facts']['path']],
                        cwd=execution, env=environment, stdout=stdout, stderr=stderr,
                        timeout=self.config['timeout_s'], check=False)
                lines = (execution / 'stdout').read_bytes().splitlines()
                require(lines, 'quality checker stdout is empty')
                reply = decode(lines[-1])
                require(isinstance(reply, dict) and set(reply) == {'status', 'report'}
                        and reply['status'] in EXIT_STATUS
                        and completed.returncode == EXIT_STATUS[reply['status']],
                        'quality checker exit and status differ')
                report_ref = reply['report']
                event = self._report(report_ref, execution / 'checker', html, row, binding)
                require(event['record']['status'] == reply['status'], 'quality stdout and report status differ')
                self.verify()
                gate.verify_result(result, call)
            except (OSError, ValueError, TypeError, KeyError, Refused, subprocess.SubprocessError) as exc:
                # Retain a bounded public UNKNOWN; raw diagnostics stay outside native context.
                save(directory / 'feedback.json', {'binding': binding, 'execution': tree_ref(execution),
                    'execution_status': 'unknown', 'report': report_ref,
                    'event': {'event_id': 'machine_quality_feedback', 'record': {
                        'visibility': 'public', 'scope': 'training', 'status': 'UNKNOWN',
                        'quality_pass': False, 'execution_status': 'unknown'}},
                    'error_type': type(exc).__name__})
                raise Refused('quality checker execution is unknown; retry is blocked') from exc
            save(directory / 'feedback.json', {'binding': binding, 'execution': tree_ref(execution),
                'execution_status': 'completed', 'report': report_ref, 'event': event})
            return event

    def _report(self, report_ref, checker, html, row, binding):
        def local(ref):
            require(Path(ref['path']).is_relative_to(checker), 'quality report evidence escapes its output')
            return read_ref(ref)

        report = decode(local(report_ref))
        require(report.get('schema_version') == 1 and report.get('status') in EXIT_STATUS,
                'quality report schema or status is invalid')
        require(hashlib.sha256(local(report['artifact'])).hexdigest() == html['sha256'],
                'quality report artifact differs from original drawing')
        require(decode(local(report['oracle'])) == decode(read_ref(row['source_facts'])),
                'quality report oracle differs from bound training facts')
        require(report['engine'] == self.config['engine'], 'quality report engine differs')
        require(isinstance(report['environment'], dict) and report['environment'], 'quality environment is missing')
        require(isinstance(report['evidence'], list) and report['evidence'], 'quality evidence is missing')
        for ref in report['evidence']:
            local(ref)
        results = report['results']
        require(isinstance(results, list) and results, 'quality viewport results are missing')
        summaries = []
        for result in results:
            viewport = result['viewport']
            require(isinstance(viewport, dict) and set(viewport) == {'width', 'height'}
                    and all(type(v) is int and v > 0 for v in viewport.values()), 'quality viewport is invalid')
            statuses, findings = [], []
            for category in ('facts', 'visual'):
                part = result.get(category)
                # A checker catch reports only viewport/status/error. It cannot pass.
                if part is None:
                    require(result['status'] == 'UNKNOWN', 'quality dimension is missing')
                    statuses.append('UNKNOWN')
                    continue
                require(isinstance(part['findings'], list), 'quality findings are invalid')
                statuses.append(part['status'])
                for finding in part['findings']:
                    require(finding.get('status') in EXIT_STATUS, 'quality finding status is invalid')
                    # Never project oracle values, raw HTML, page errors or arbitrary diagnostic objects.
                    if len(findings) < 8:
                        findings.append(public_finding(category, finding))
                    require(not (finding['status'] == 'FAIL' and part['status'] != 'FAIL')
                            and not (finding['status'] == 'UNKNOWN' and part['status'] == 'PASS'),
                            'quality dimension erased a blocking finding')
            for finding in result.get('environmentFindings', []):
                statuses.append(finding['status'])
                if len(findings) < 8:
                    findings.append({'category': 'environment', 'status': finding['status'],
                                     'kind': str(finding.get('kind', 'unspecified'))[:120]})
            require(aggregate(statuses) == result['status'], 'quality viewport status differs')
            if len(summaries) < 4:
                summaries.append({'viewport': viewport, 'status': result['status'], 'findings': findings,
                    'facts_status': (result.get('facts') or {}).get('status', 'UNKNOWN'),
                    'visual_status': (result.get('visual') or {}).get('status', 'UNKNOWN')})
        require(aggregate([r['status'] for r in results]) == report['status'], 'quality report erased a blocking result')
        return {'event_id': 'machine_quality_feedback', 'record': {
            'visibility': 'public', 'scope': 'training', 'execution_status': 'completed',
            'status': report['status'], 'quality_pass': report['status'] == 'PASS',
            'call_sha256': binding['call_sha256'], 'task_id': binding['call']['inputs']['task_id'],
            'run_id': binding['call']['run_id'],
            'result': {'path': binding['result']['run_dir'], 'sha256': binding['result']['output_sha256']},
            'source': row['source'], 'harness': binding['call']['inputs']['harness'],
            'artifact': html, 'report': report_ref, 'results': summaries}}
