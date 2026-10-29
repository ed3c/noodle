#!/usr/bin/env python3
"""Private stdio MCP witness for the control-noodle native release drive."""
import datetime
import json
import os
from pathlib import Path
import sys

record = Path(sys.argv[1])

def observe(event, **values):
    with record.open('a') as stream:
        stream.write(json.dumps({'event': event, 'pid': os.getpid(), 'parent': os.getppid(),
                                 'at': datetime.datetime.now(datetime.timezone.utc).isoformat(), **values}) + '\n')
        stream.flush()
        os.fsync(stream.fileno())

observe('started')
try:
    for line in sys.stdin:
        request = json.loads(line)
        method = request.get('method')
        if 'id' not in request:
            continue
        if method == 'initialize':
            result = {'protocolVersion': request.get('params', {}).get('protocolVersion', '2024-11-05'),
                      'capabilities': {'tools': {}}, 'serverInfo': {'name': 'control-noodle-release-witness', 'version': '1'}}
        elif method == 'tools/list':
            result = {'tools': [{'name': 'ping', 'description': 'Return the private release verification sentinel.',
                                'annotations': {'readOnlyHint': False, 'destructiveHint': False, 'openWorldHint': False},
                                'inputSchema': {'type': 'object', 'properties': {}, 'additionalProperties': False}}]}
        elif method == 'tools/call' and request.get('params', {}).get('name') == 'ping':
            observe('ping')
            result = {'content': [{'type': 'text', 'text': 'CONTROL_NOODLE_MCP_OBSERVED'}], 'isError': False}
        elif method in ('resources/list', 'resources/templates/list', 'prompts/list'):
            result = { {'resources/list': 'resources', 'resources/templates/list': 'resourceTemplates', 'prompts/list': 'prompts'}[method]: []}
        elif method == 'ping':
            result = {}
        else:
            print(json.dumps({'jsonrpc': '2.0', 'id': request['id'], 'error': {'code': -32601, 'message': method}}), flush=True)
            continue
        print(json.dumps({'jsonrpc': '2.0', 'id': request['id'], 'result': result}), flush=True)
finally:
    observe('stdio_closed')
