"""GET-only settlement controls use synthetic provider responses, never real keys."""
import copy
import io
import json
import tempfile
import unittest
from dataclasses import replace
from decimal import Decimal
from pathlib import Path
from unittest.mock import patch

from openrouter_billing import (
    BILLED_MODEL,
    OpenRouterBilling,
    ReadOnlyOpenRouter,
    verify_settlement,
)
from owner_contract import (
    OwnerCall,
    OwnerResult,
    Refused,
    SharedGate,
    UnknownEffect,
    tree_ref,
)
from soodles_bridge import MODELS, decode, reference, save


class FakeReadOnlyProvider:
    def __init__(self):
        self.requests = []
        self.key_reads = 0
        self.after = '0.387106123'
        self.delay = False
        self.model = BILLED_MODEL
        self.fingerprint = 'f' * 64

    def get(self, resource, *, generation_id=None):
        self.requests.append((resource, generation_id))
        if resource == 'key':
            self.key_reads += 1
            usage = '0.087106123' if self.key_reads == 1 or (self.delay and self.key_reads == 2) else self.after
            data = {'limit': '5', 'usage': usage, 'limit_remaining': str(Decimal('5') - Decimal(usage)),
                    'limit_reset': None, 'is_management_key': False}
        else:
            data = {'id': generation_id, 'model': self.model, 'total_cost': '0.1' if generation_id == 'gen-one' else '0.2',
                    'is_byok': False, 'finish_reason': 'stop', 'cancelled': False}
        return {'resource': resource, 'generation_id': generation_id, 'data': data,
                'key_fingerprint': self.fingerprint, 'response_sha256': 'e' * 64,
                'observed_at': '2026-10-28T09:00:00+00:00'}


class BillingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.client = FakeReadOnlyProvider()
        self.billing = OpenRouterBilling(self.client, initial_usage_usd='0.087106123',
                                         exclusive_key=True, observations=2, interval_s=0)
        self.call = OwnerCall('pi', 'run-1', 'solver', 'pi', MODELS['pi'], {'fixture_only': True})
        result = self.root / 'result'
        result.mkdir()
        original = self.root / 'capture'
        original.mkdir()
        raw = original / 'session.jsonl'
        rows = [{'type': 'message', 'message': {'role': 'assistant', 'provider': 'openrouter',
                 'model': MODELS['pi'].removeprefix('openrouter/'), 'responseId': identity,
                 'content': [{'type': 'text', 'text': 'fixture only'}]}} for identity in ('gen-one', 'gen-two')]
        raw.write_text(''.join(json.dumps(row) + '\n' for row in rows))
        descriptor = save(original / 'descriptor.json', {'provider': 'pi', 'raw': reference(raw),
                          'terminal': {'line': 2}, 'gaps': []})
        evidence = result / 'evidence'
        evidence.mkdir()
        custody = []
        for ref in (descriptor, reference(raw)):
            target = evidence / ref['sha256']
            target.write_bytes(Path(ref['path']).read_bytes())
            custody.append({'original': ref, 'saved': reference(target)})
        save(evidence / 'index.json', custody)
        save(result / 'native-evidence.json', {'descriptor': descriptor})
        receipt = save(self.root / 'owner-receipt.json', {'fixture_only': True})
        self.result = OwnerResult(str(result), tree_ref(result)['sha256'], 'fixture', None,
                                  receipt['path'], receipt['sha256'], True, None, None)

    def test_actual_decimal_bills_reconcile_and_release_hold_without_reexecution(self):
        before = self.billing.begin(self.call, self.root / 'before')
        owner_calls = []

        class Owner:
            def execute(inner, call):
                owner_calls.append(call)
                return self.result

        gate = SharedGate(self.root / 'gate.json', Owner(), initial_paid_spent_usd='0.087106123')
        gate.execute(self.call)
        self.assertEqual(gate._spent(), Decimal('5'))
        settlement = self.billing.settle(self.call, before, self.result, self.root / 'after')
        actual = gate.reconcile_billing(self.call, settlement)
        self.assertEqual(actual.cost_usd, '0.3')
        self.assertEqual(gate._spent(), Decimal('0.387106123'))
        self.assertEqual(len(owner_calls), 1)
        self.assertTrue(gate.execute(self.call).cached)
        self.assertEqual(gate.reconcile_billing(self.call, settlement), actual)
        self.assertEqual(set(r for r, _ in self.client.requests), {'key', 'generation'})
        self.assertEqual(len(owner_calls), 1)

    def test_automatic_settlement_cannot_claim_another_calls_generation(self):
        before = self.billing.begin(self.call, self.root / 'before')
        first_bill = self.billing.settle(self.call, before, self.result, self.root / 'first-bill')
        delivered = [replace(self.result, cost_usd='0.3', billing_receipt=first_bill)]

        class Owner:
            def execute(inner, call):
                return delivered[-1]

        gate = SharedGate(self.root / 'gate.json', Owner(), initial_paid_spent_usd='0.087106123')
        gate.execute(self.call)
        second = replace(self.call, run_id='second')
        second_before = self.billing.begin(second, self.root / 'second-before')
        self.client.after = '0.687106123'
        duplicate_bill = self.billing.settle(second, second_before, self.result, self.root / 'duplicate-bill')
        delivered.append(replace(self.result, cost_usd='0.3', billing_receipt=duplicate_bill))
        with self.assertRaises(UnknownEffect):
            gate.execute(second)
        self.assertTrue(gate.state['blocked'])
        with self.assertRaises(UnknownEffect):
            gate.execute(replace(second, run_id='no-fallback'))

    def test_delayed_key_billing_is_read_only_and_bounded(self):
        self.client.delay = True
        before = self.billing.begin(self.call, self.root / 'before')
        ref = self.billing.settle(self.call, before, self.result, self.root / 'after')
        receipt = decode(Path(ref['path']).read_bytes())
        self.assertEqual(receipt['status'], 'settled')
        self.assertTrue((self.root / 'after/unknown-000.json').exists())
        self.assertEqual(self.client.key_reads, 3)

    def test_other_consumption_retains_hold_then_same_call_can_reconcile(self):
        before = self.billing.begin(self.call, self.root / 'before')
        self.client.after = '0.487106123'
        ref = self.billing.settle(self.call, before, self.result, self.root / 'after')
        self.assertEqual(decode(Path(ref['path']).read_bytes())['status'], 'unknown')
        self.assertEqual(self.billing.pending, self.call.fingerprint())
        with self.assertRaises(Refused):
            self.billing.begin(replace(self.call, run_id='next'), self.root / 'no-next')
        with self.assertRaises(Refused):
            verify_settlement(self.call, self.result, ref)
        self.client.after = '0.387106123'
        later = self.billing.settle(self.call, before, self.result, self.root / 'later')
        self.assertEqual(verify_settlement(self.call, self.result, later)['cost_usd'], '0.3')
        self.assertIsNone(self.billing.pending)

    def test_wrong_route_and_changed_receipt_refuse(self):
        before = self.billing.begin(self.call, self.root / 'before')
        self.client.model = 'foreign/model'
        ref = self.billing.settle(self.call, before, self.result, self.root / 'wrong')
        self.assertEqual(decode(Path(ref['path']).read_bytes())['status'], 'unknown')
        self.client.model = BILLED_MODEL
        settled = self.billing.settle(self.call, before, self.result, self.root / 'fixed')
        with self.assertRaises(Refused):
            verify_settlement(replace(self.call, run_id='foreign'), self.result, settled)
        receipt = decode(Path(settled['path']).read_bytes())
        Path(receipt['observation']['path']).write_text('{}')
        with self.assertRaises(Refused):
            verify_settlement(self.call, self.result, settled)

    def test_no_exclusivity_or_foreign_initial_usage_refuses_before_dispatch(self):
        with self.assertRaises(Refused):
            OpenRouterBilling(self.client, initial_usage_usd='0', exclusive_key=False)
        billing = OpenRouterBilling(self.client, initial_usage_usd='0', exclusive_key=True)
        with self.assertRaisesRegex(Refused, 'outside the original serial owner'):
            billing.begin(self.call, self.root / 'before')
        self.assertFalse((self.root / 'before').exists())

    def test_private_supplier_only_gets_exact_endpoints_without_persisting_key(self):
        environment = self.root / '.env'
        token = 'sk-or-v1-fixture-not-a-real-key'
        environment.write_text('OPENROUTER_API_KEY=' + token + '\n')
        environment.chmod(0o600)
        client = ReadOnlyOpenRouter(environment)
        body = json.dumps({'data': {'usage': 0.087106123, 'limit': 5,
                         'limit_remaining': 4.912893877, 'limit_reset': None,
                         'label': 'must-not-be-persisted'}}).encode()
        requests = []

        class Opener:
            def open(self, request, timeout):
                requests.append(request)
                return io.BytesIO(body)

        with patch('openrouter_billing.urllib.request.build_opener', return_value=Opener()):
            result = client.get('key')
            with self.assertRaises(Refused):
                client.get('chat/completions')
        self.assertEqual(requests[0].method, 'GET')
        self.assertEqual(requests[0].full_url, 'https://openrouter.ai/api/v1/key')
        self.assertEqual(result['data']['usage'], '0.087106123')
        self.assertNotIn(token, json.dumps(result))
        self.assertNotIn('must-not-be-persisted', json.dumps(result))
        self.assertNotIn(token, repr(client))

    def test_reconcile_unknown_never_changes_saved_accounting(self):
        before = self.billing.begin(self.call, self.root / 'before')
        self.client.after = '0.087106123'
        unknown = self.billing.settle(self.call, before, self.result, self.root / 'unknown')

        class Owner:
            def execute(inner, call):
                return self.result

        gate = SharedGate(self.root / 'gate.json', Owner(), initial_paid_spent_usd='0.087106123')
        gate.execute(self.call)
        previous = copy.deepcopy(gate.state)
        with self.assertRaises(Refused):
            gate.reconcile_billing(self.call, unknown)
        self.assertEqual(gate.state, previous)
        self.assertEqual(gate._spent(), Decimal('5'))


if __name__ == '__main__':
    unittest.main()
