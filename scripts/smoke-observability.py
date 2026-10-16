#!/usr/bin/env python3
"""Find a failed event, inspect metrics/trace, replay it, and inspect the live dashboard."""
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import base64
import datetime
import json
import os
from pathlib import Path
import runpy
import signal
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from smoke_db import isolated_database, receiver_counts

ROOT = Path(__file__).resolve().parents[1]
helpers = runpy.run_path(str(ROOT / 'scripts/smoke-worker.py'))
free_port, admin = helpers['free_port'], helpers['admin']


def main():
    # Matches the provisioned local Prometheus targets; refuse to steal existing listeners.
    for port in [9091, 8081]:
        import socket
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', port))
    account = admin('tenant-create', '--name', 'Observability smoke tenant')
    children, logs, captured = [], [], []
    marker = 'observability-private-payload'
    api_address, receiver_address = f'127.0.0.1:{free_port()}', f'127.0.0.1:{free_port()}'

    class Proxy(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers['Content-Length']))
            assert all(name not in self.headers for name in ['traceparent', 'tracestate', 'baggage', 'Authorization'])
            captured.append(body)
            if len(captured) == 1:
                self.send_response(400)
                self.end_headers()
                return
            headers = {name: self.headers[name] for name in
                       ['webhook-id', 'webhook-timestamp', 'webhook-signature', 'Content-Type']}
            request = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                             data=body, headers=headers, method='POST')
            with urllib.request.urlopen(request, timeout=3) as response:
                assert response.status == 204
            self.send_response(204)
            self.end_headers()

    proxy = ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
    thread = threading.Thread(target=proxy.serve_forever, daemon=True)
    thread.start()
    env = dict(os.environ, API_ADDR=api_address, API_OPS_ADDR='127.0.0.1:9091',
               WORKER_ADDR='127.0.0.1:8081', DEV_RECEIVER_URL=f'http://localhost:{proxy.server_port}/webhooks',
               WORKER_CONCURRENCY='1', QUEUE_POLL_INTERVAL='20ms', RATE_LIMIT_ENABLED='false',
               TRACE_ENDPOINT='http://127.0.0.1:14318/v1/traces', TRACE_SAMPLE_RATIO='1')
    grafana_password = (os.environ.get('GRAFANA_ADMIN_PASSWORD') or 'relayforge-local-only')
    grafana_auth = 'Basic ' + base64.b64encode(('admin:' + grafana_password).encode()).decode()

    def get(address, path, headers=None):
        req = urllib.request.Request('http://' + address + path, headers=headers or {})
        try:
            response = urllib.request.urlopen(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read()

    def wait(predicate, message, timeout=25):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                value = predicate()
                if value:
                    return value
            except (OSError, KeyError, ValueError):
                pass
            time.sleep(.1)
        raise RuntimeError(message)

    def call(method, path, data=None, key=None):
        headers = {'Authorization': 'Bearer ' + account['api_key'], 'Content-Type': 'application/json',
                   'baggage': 'secret=observability-private-baggage',
                   'traceparent': '00-11111111111111111111111111111111-2222222222222222-01'}
        if key:
            headers['Idempotency-Key'] = key
        request = urllib.request.Request('http://' + api_address + path,
                                         data=None if data is None else json.dumps(data).encode(),
                                         headers=headers, method=method)
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.headers.get('X-Request-ID')
            return response.status, json.load(response), response.headers

    def query(expr):
        code, raw = get('127.0.0.1:19090', '/api/v1/query?' + urllib.parse.urlencode({'query': expr}))
        result = json.loads(raw)
        assert code == 200 and result['status'] == 'success'
        return result['data']['result']

    def has_value(expr, expected):
        return any(float(row['value'][1]) == expected for row in query(expr))

    def delivery(state):
        records = call('GET', '/v1/deliveries')[1]['data']
        return records[0] if len(records) == 1 and records[0]['status'] == state else None

    def stop(child):
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        assert child.wait(timeout=20) == 0

    try:
        wait(lambda: get('127.0.0.1:19090', '/-/ready')[0] == 200, 'start make dev-observe-up before this smoke')
        wait(lambda: get('127.0.0.1:13000', '/api/health')[0] == 200, 'Grafana not ready')
        wait(lambda: get('127.0.0.1:16686', '/')[0] == 200, 'Jaeger not ready')
        with ExitStack() as stack:
            def launch(role, child_env):
                log = stack.enter_context(tempfile.TemporaryFile())
                child = subprocess.Popen([str(ROOT / 'bin' / role)], env=child_env,
                                         stdout=log, stderr=log)
                children.append(child)
                logs.append(log)
                return child

            launch('api', env)
            wait(lambda: get(api_address, '/health/ready')[0] == 200, 'API not ready')
            assert get(api_address, '/metrics')[0] == 404
            assert get('127.0.0.1:9091', '/metrics')[0] == 200
            _, created, _ = call('POST', '/v1/endpoints',
                                   {'url': env['DEV_RECEIVER_URL'], 'subscribed_event_types': ['order.created']})
            launch('receiver', dict(env, RECEIVER_ADDR=receiver_address,
                                   RECEIVER_SECRETS=created['signing_secret']))
            wait(lambda: get(receiver_address, '/health/ready')[0] == 200, 'receiver not ready')
            _, accepted, headers = call('POST', '/v1/events',
                                         {'type': 'order.created', 'data': {'marker': marker}}, 'observability-event')
            assert headers['X-Trace-ID'] != '11111111111111111111111111111111'
            launch('worker', env)
            failed = wait(lambda: delivery('dead_letter'), 'permanent failure not persisted')
            _, detail, _ = call('GET', '/v1/events/' + accepted['event_id'])
            trace_id = detail['trace_id']
            assert trace_id == headers['X-Trace-ID']
            _, attempts, _ = call('GET', '/v1/deliveries/' + failed['id'] + '/attempts')
            assert attempts['data'][0]['http_status'] == 400 and attempts['data'][0]['outcome'] == 'permanent_failure'
            wait(lambda: has_value('max(relayforge_queue_deliveries{status="dead_letter"})', 1),
                 'Prometheus did not observe dead-letter backlog; check host.docker.internal access')
            def jaeger_spans(path):
                code, body = get('127.0.0.1:16686', path)
                if code != 200:
                    return []
                # v3 uses OTLP JSON and may stream several JSON objects.
                spans = []
                decoder = json.JSONDecoder()
                text = body.decode().strip()
                while text:
                    record, end = decoder.raw_decode(text)
                    for resource in record.get('result', {}).get('resourceSpans', []):
                        for scope in resource.get('scopeSpans', []):
                            spans.extend(scope.get('spans', []))
                    text = text[end:].lstrip()
                return spans
            wait(lambda: any(span.get('traceId') == trace_id for span in jaeger_spans('/api/v3/traces/' + trace_id)),
                 'acceptance trace not found in Jaeger')
            _, replay, _ = call('POST', '/v1/deliveries/' + failed['id'] + '/replay', {}, 'observability-repair')
            wait(lambda: delivery('delivered'), 'operator replay did not deliver')
            _, retry_receipt, _ = call('POST', '/v1/deliveries/' + failed['id'] + '/replay', {}, 'observability-repair')
            assert retry_receipt == replay
            wait(lambda: has_value('max(relayforge_queue_deliveries{status="delivered"})', 1), 'delivered gauge not observed')
            wait(lambda: has_value('sum(relayforge_replays_total{result="accepted"})', 1), 'replay counter not observed')
            assert len(captured) == 2 and captured[0] == captured[1] and receiver_counts() == (1, 1)
            code, raw = get('127.0.0.1:13000', '/api/dashboards/uid/relayforge-operations', {'Authorization': grafana_auth})
            dashboard = json.loads(raw)['dashboard']
            assert code == 200 and len(dashboard['panels']) == 16
            for panel in dashboard['panels']:
                for target in panel['targets']:
                    query(target['expr'])  # Parses/evaluates every provisioned panel query.
            code, raw = get('127.0.0.1:19090', '/api/v1/targets')
            assert code == 200 and all(row['health'] == 'up' for row in json.loads(raw)['data']['activeTargets'])
            # Flush traces before inspecting all worker spans and their acceptance links.
            for child in reversed(children):
                stop(child)
            children.clear()
            now = datetime.datetime.now(datetime.timezone.utc)
            search = '/api/v3/traces?' + urllib.parse.urlencode({
                'query.service_name': 'relayforge-worker',
                'query.start_time_min': (now - datetime.timedelta(hours=1)).isoformat().replace('+00:00', 'Z'),
                'query.start_time_max': now.isoformat().replace('+00:00', 'Z')})
            dispatch_spans = [span for span in jaeger_spans(search)
                              if span.get('name') == 'delivery.dispatch'
                              and any(tag['key'] == 'event_id' and tag['value'].get('stringValue') == accepted['event_id']
                                      for tag in span.get('attributes', []))]
            assert len(dispatch_spans) == 2
            assert all(any(link['traceId'] == trace_id for link in span.get('links', [])) for span in dispatch_spans)
            metric_raw = query('sum(relayforge_dispatch_attempts_total)')
            assert metric_raw
            for log in logs:
                log.seek(0)
                output = log.read().decode()
                assert all(value not in output for value in
                           [marker, 'observability-private-baggage', account['api_key'], created['signing_secret'], env['ENCRYPTION_KEY']])
            print('PASS: operator found failed event/400 history, observed dead letter in Prometheus, replayed once and observed delivery')
            print('PASS: live Grafana dashboard and all 16 PromQL panels evaluated; Jaeger linked both dispatches to original acceptance')
            print('PASS: private metrics, no receiver trace/baggage/auth leakage, immutable replay bytes, one business effect and clean logs')
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
        proxy.shutdown()
        proxy.server_close()
        thread.join(timeout=5)
        admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    with isolated_database():
        main()
