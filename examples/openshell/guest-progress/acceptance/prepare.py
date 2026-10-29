#!/usr/bin/env python3
"""Prepare fresh no-provider native fixture inputs; never starts a gateway/VM."""
import argparse, hashlib, json, os, socket, subprocess, tempfile
from pathlib import Path


def sha(path):
    with open(path, 'rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--parent', required=True)
    parser.add_argument('--gateway', required=True)
    parser.add_argument('--gateway-sha256', required=True)
    parser.add_argument('--gateway-version', required=True)
    parser.add_argument('--cli', required=True)
    parser.add_argument('--driver-dir', required=True)
    parser.add_argument('--fixture', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    gateway, cli, fixture = [Path(p).resolve(strict=True) for p in (args.gateway,args.cli,args.fixture)]
    if sha(gateway) != args.gateway_sha256:
        raise SystemExit('gateway bytes differ from reviewed binary')
    parent = Path(args.parent).resolve(strict=True)
    root = Path(tempfile.mkdtemp(prefix='ngp-',dir=parent))
    if len(str(root/'vm')) > 75:
        raise SystemExit(f'VM fixture state path too long; preserved {root}')
    for name in ('home','state','vm','custody','host','client'):
        (root/name).mkdir(mode=0o700)
    with socket.socket() as probe:
        probe.bind(('127.0.0.1',0))
        port = probe.getsockname()[1]
    endpoint = f'https://127.0.0.1:{port}'
    name = root.name
    env = {'HOME':str(root/'home'),'PATH':'/opt/homebrew/opt/e2fsprogs/sbin:/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin','XDG_CONFIG_HOME':str(root/'client'),'XDG_STATE_HOME':str(root/'state'),'OPENSHELL_DB_URL':f'sqlite://{root}/state/gateway.db?mode=rwc','TOKIO_WORKER_THREADS':'2'}
    calls = []
    def call(label,argv,extra=None):
        result = subprocess.run([str(a) for a in argv],cwd=root,env={**env,**(extra or {})},stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=60)
        log=root/(label+'.log');log.write_bytes(result.stdout)
        calls.append({'label':label,'exit_code':result.returncode,'log':str(log),'sha256':sha(log)})
        (root/'preparation.json').write_text(json.dumps({'gateway_started':False,'calls':calls},indent=2)+'\n')
        if result.returncode: raise SystemExit(f'{label} failed; evidence at {root}')
    tls=root/'tls'
    call('certs',[gateway,'generate-certs','--output-dir',tls,'--server-san',name,'--server-san','127.0.0.1'])
    config=root/'gateway.toml'
    # JSON strings are also valid TOML basic string literals for these paths.
    q=lambda value:json.dumps(str(value))
    config.write_text(f'''[openshell]
version = 2
[openshell.gateway]
name = {q(name)}
bind_address = {q(f'127.0.0.1:{port}')}
compute_driver = "vm"
guest_tls_ca = {q(tls/'ca.crt')}
[openshell.gateway.credential_storage]
key_encryption_key_path = {q(root/'empty-credential-key.bin')}
[openshell.gateway.gateway_jwt]
signing_key_path = {q(tls/'jwt/signing.pem')}
public_key_path = {q(tls/'jwt/public.pem')}
kid_path = {q(tls/'jwt/kid')}
gateway_id = {q(name)}
[openshell.drivers.vm]
driver_dir = {q(Path(args.driver_dir).resolve(strict=True))}
state_dir = {q(root/'vm')}
default_image = "nvcr.io/nvidia/base/ubuntu:24.04"
vcpus = 2
mem_mib = 2048
''')
    call('client',[cli,'gateway','add',endpoint,'--name',name,'--local'],{'OPENSHELL_LOCAL_TLS_DIR':str(tls)})
    argv=[str(gateway),'--config',str(config),'--tls-cert',str(tls/'server/tls.crt'),'--tls-key',str(tls/'server/tls.key'),'--tls-client-ca',str(tls/'ca.crt')]
    call('preflight',[gateway,'config','preflight','--',*argv[1:]])
    metadata=root/'client/openshell/gateways'/name/'metadata.json'
    spec={'template':{'image':'nvcr.io/nvidia/base/ubuntu:24.04'},'command':['/bin/sh','-c','while :; do sleep 3600; done'],'providers':[],'policy':{'version':1,'filesystem':{'include_workdir':True,'read_only':['/bin','/usr','/lib','/proc','/dev/urandom','/etc','/var/log'],'read_write':['/tmp','/dev/null']},'landlock':{'compatibility':'best_effort'},'network_policies':{}}}
    specfile=root/'sandbox-spec.json';specfile.write_text(json.dumps(spec,indent=2)+'\n')
    inputs={'schema':1,'root':str(root),'name':name,'endpoint':endpoint,'gateway_version':args.gateway_version,'gateway_sha256':sha(gateway),'cli':str(cli),'cli_sha256':sha(cli),'fixture':str(fixture),'fixture_sha256':sha(fixture),'config':str(config),'config_sha256':sha(config),'spec':str(specfile),'spec_sha256':sha(specfile),'client_metadata':str(metadata),'client_metadata_sha256':sha(metadata),'environment':[f'{k}={v}' for k,v in env.items()],'resident':{'name':name,'argv':argv,'directory':str(root),'environment':[f'{k}={v}' for k,v in env.items()],'sharing':'shared','transport':'network'}}
    (root/'inputs.json').write_text(json.dumps(inputs,indent=2)+'\n')
    print(json.dumps({'status':'prepared_not_started','inputs':str(root/'inputs.json'),'fixture_sha256':inputs['fixture_sha256'],'providers':0}))

if __name__ == '__main__': main()
