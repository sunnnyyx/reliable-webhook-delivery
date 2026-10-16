#!/usr/bin/env python3
"""Copy reviewed configuration into a project-owned Docker volume, without host bind mounts."""
from pathlib import Path
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[1]
VOLUME = 'relayforge_observability_config'
helper = 'relayforge-assets-' + uuid.uuid4().hex


def run(*arguments):
    result = subprocess.run(['docker', *arguments], capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError('observability asset operation failed; check Docker availability')


run('volume', 'create', '--label', 'com.docker.compose.project=relayforge', VOLUME)
try:
    run('create', '--name', helper, '-v', VOLUME + ':/bundle', '--entrypoint', '/bin/true', 'redis:8.10.2-alpine')
    run('cp', str(ROOT / 'deploy/observability') + '/.', helper + ':/bundle/')
    print('Prepared reviewed observability configuration in the project Docker volume')
finally:
    subprocess.run(['docker', 'rm', '-f', helper], capture_output=True, timeout=30)
