"""Disposable schemas for binary smoke tests against this project's local Compose DB."""
from contextlib import contextmanager
import os
from pathlib import Path
import subprocess
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit
import uuid
import socket

ROOT = Path(__file__).resolve().parents[1]


@contextmanager
def isolated_database():
    original = os.environ.get('DATABASE_URL', '')
    url = urlsplit(original)
    if (os.environ.get('PROFILE', 'development') != 'development'
            or url.hostname not in ('127.0.0.1', 'localhost')
            or url.path != '/relayforge_dev'
            or url.username != 'relayforge'
            or url.port != int(os.environ.get('POSTGRES_HOST_PORT', '5432'))):
        raise RuntimeError('binary smoke tests require this project local development Compose database')
    query = dict(parse_qsl(url.query, keep_blank_values=True))
    if any(key != 'sslmode' for key in query):
        raise RuntimeError('smoke schema isolation requires an unscoped development DATABASE_URL')
    schema = 'rf_smoke_' + uuid.uuid4().hex
    compose = ['docker', 'compose', '--env-file', str(ROOT / '.env'),
               '-f', str(ROOT / 'deploy/compose.yaml'), 'exec', '-T', 'postgres',
               'psql', '-U', 'relayforge', '-d', 'relayforge_dev',
               '-v', 'ON_ERROR_STOP=1', '-c']

    def sql(statement):
        result = subprocess.run([*compose, statement], capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError('isolated smoke schema operation failed; database details withheld')

    sql('CREATE SCHEMA ' + schema)
    receiver_schema = schema + '_receiver'
    ops_original = os.environ.get('API_OPS_ADDR')
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        os.environ['API_OPS_ADDR'] = f'127.0.0.1:{listener.getsockname()[1]}'
    receiver_original = os.environ.get('RECEIVER_DATABASE_URL')
    source_original = os.environ.get('RECEIVER_SOURCE')
    try:
        sql('CREATE SCHEMA ' + receiver_schema)
        query['search_path'] = schema
        os.environ['DATABASE_URL'] = urlunsplit(url._replace(query=urlencode(query)))
        result = subprocess.run([str(ROOT / 'bin/migrate'), 'up'], cwd=ROOT,
                                capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError('isolated smoke schema migrations failed')
        receiver_query = dict(query, search_path=receiver_schema)
        os.environ['RECEIVER_DATABASE_URL'] = urlunsplit(url._replace(query=urlencode(receiver_query)))
        os.environ['RECEIVER_SOURCE'] = 'smoke-source'
        result = subprocess.run([str(ROOT / 'bin/migrate'), 'receiver-up'], cwd=ROOT,
                                capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError('isolated receiver schema migrations failed')
        yield
    finally:
        os.environ['DATABASE_URL'] = original
        for key, value in [('RECEIVER_DATABASE_URL', receiver_original), ('RECEIVER_SOURCE', source_original), ('API_OPS_ADDR', ops_original)]:
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        sql('DROP SCHEMA IF EXISTS ' + receiver_schema + ' CASCADE')
        sql('DROP SCHEMA ' + schema + ' CASCADE')


def receiver_counts():
    """Inspect only the generated disposable receiver schema, never a user namespace."""
    import re
    schema = dict(parse_qsl(urlsplit(os.environ['RECEIVER_DATABASE_URL']).query)).get('search_path', '')
    if not re.fullmatch(r'rf_smoke_[0-9a-f]{32}_receiver', schema):
        raise RuntimeError('receiver counts require a generated disposable schema')
    statement = (f'SELECT (SELECT count(*) FROM {schema}.receiver_processed_events),'
                 f'(SELECT count(*) FROM {schema}.receiver_business_events)')
    result = subprocess.run(['docker', 'compose', '--env-file', str(ROOT / '.env'),
                             '-f', str(ROOT / 'deploy/compose.yaml'), 'exec', '-T', 'postgres',
                             'psql', '-U', 'relayforge', '-d', 'relayforge_dev', '-At',
                             '-v', 'ON_ERROR_STOP=1', '-c', statement],
                            capture_output=True, timeout=10)
    if result.returncode:
        raise RuntimeError('receiver count query failed; database details withheld')
    return tuple(map(int, result.stdout.decode().strip().split('|')))
