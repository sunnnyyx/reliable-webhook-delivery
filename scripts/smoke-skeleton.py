#!/usr/bin/env python3
"""Exercise built API/worker processes against the configured real database."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

from smoke_db import isolated_database

ROOT = Path(__file__).resolve().parents[1]


def probe(url):
    with urllib.request.urlopen(url, timeout=2) as response:
        body = json.load(response)
        assert response.status == 200
        assert len(body['request_id']) == 32
        assert response.headers['X-Request-ID'] == body['request_id']
        return body['status']


def run(role):
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        address = f'127.0.0.1:{listener.getsockname()[1]}'
    env = os.environ.copy()
    env['API_ADDR' if role == 'api' else 'WORKER_ADDR'] = address
    env['SHUTDOWN_TIMEOUT'] = '2s'
    with tempfile.TemporaryFile() as logs:
        process = subprocess.Popen([str(ROOT / 'bin' / role)], env=env,
                                   stdout=logs, stderr=logs)
        try:
            deadline = time.monotonic() + 10
            while True:
                if process.poll() is not None:
                    raise RuntimeError(f'{role} exited before readiness')
                try:
                    assert probe(f'http://{address}/health/ready') == 'ready'
                    break
                except (urllib.error.URLError, TimeoutError):
                    if time.monotonic() >= deadline:
                        raise RuntimeError(f'{role} readiness deadline exceeded')
                    time.sleep(0.05)
            assert probe(f'http://{address}/health/live') == 'live'
            process.send_signal(signal.SIGTERM)
            assert process.wait(timeout=5) == 0
            logs.seek(0)
            records = [json.loads(line) for line in logs]
            assert any(record.get('msg') == 'started' for record in records)
            assert any(record.get('msg') == 'stopped' for record in records)
            print(f'PASS: {role} readiness, liveness, request IDs, JSON logs, SIGTERM shutdown')
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)


if __name__ == '__main__':
    if not os.environ.get('DATABASE_URL'):
        raise SystemExit('DATABASE_URL is required; run make test-smoke')
    with isolated_database():
        for role in ('api', 'worker'):
            run(role)
