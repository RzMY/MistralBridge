"""Verify historical statistics in an isolated CPA Docker container on Linux."""
import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import shutil
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid

from check_plugin import check_plugin


def run(args):
    root = Path(args.work_dir).resolve()
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    auths = root / 'auths'
    plugins = auths / 'plugins/linux/amd64'
    data = auths / 'plugin-data/mistral-bridge'
    plugins.mkdir(parents=True)
    data.mkdir(parents=True)
    shutil.copy2(args.library, plugins / 'mistral-bridge.so')
    check_plugin(args.library, args.version)
    calls = []

    class Mock(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            assert self.headers.get('Authorization') == 'Bearer mock-telemetry'
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            calls.append(body.get('stream', False))
            usage = {'prompt_tokens': 10, 'completion_tokens': 5, 'total_tokens': 15, 'prompt_tokens_details': {'cached_tokens': 4}}
            if body['model'] == 'reject-upstream':
                payload = b'{"message":"mock rejected request"}'
                self.send_response(400)
                kind = 'application/json'
            elif body.get('stream'):
                events = [
                    {'id':'mock','object':'chat.completion.chunk','model':body['model'],'choices':[{'index':0,'delta':{'content':'OK'}}]},
                    {'id':'mock','object':'chat.completion.chunk','model':body['model'],'choices':[{'index':0,'delta':{},'finish_reason':'stop'}],'usage':usage},
                ]
                payload = (''.join('data: '+json.dumps(e)+'\n\n' for e in events)+'data: [DONE]\n\n').encode()
                kind = 'text/event-stream'
                self.send_response(200)
            else:
                payload = json.dumps({'id':'mock','object':'chat.completion','model':body['model'],'choices':[{'index':0,'message':{'role':'assistant','content':'OK'},'finish_reason':'stop'}],'usage':usage}).encode()
                kind = 'application/json'
                self.send_response(200)
            self.send_header('Content-Type',kind)
            self.send_header('Content-Length',str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    upstream = ThreadingHTTPServer(('127.0.0.1',0),Mock)
    threading.Thread(target=upstream.serve_forever,daemon=True).start()
    with socket.socket() as reserved:
        reserved.bind(('127.0.0.1',0))
        port = reserved.getsockname()[1]
    config = {
        'config-version':8, 'server':{'host':'127.0.0.1','port':port},
        'access':{'api-keys':['telemetry-test-client']},
        'routing':{'retry':{'request-retry':0,'max-retry-interval':0}},
        'management':{'allow-remote':False,'secret-key':'telemetry-test-management','disable-control-panel':True},
        'plugins':{'enabled':True,'dir':'/root/.cli-proxy-api/plugins','configs':{'mistral-bridge':{'enabled':True}}},
    }
    config_path = root/'config.yaml'
    config_path.write_text(json.dumps(config))
    state = {
        'keys':[{'id':'test','label':'Telemetry test','secret':'mock-telemetry','base_url':f'http://127.0.0.1:{upstream.server_port}/v1','enabled':True}],
        'models':[{'id':name,'client_model':name,'upstream_model':target,'enabled':True} for name,target in [('telemetry-test','test-upstream'),('telemetry-error','reject-upstream')]],
        'catalog':[],
    }
    (data/'state.json').write_text(json.dumps(state))
    now = datetime.now(timezone.utc).isoformat().replace('+00:00','Z')
    with (data/'requests.jsonl').open('w') as out:
        for i in range(10005):
            out.write(json.dumps({'id':f'{i:024x}','model':'legacy','status':'ok','created_at':now,'input_tokens':10,'output_tokens':5,'total_tokens':15,'usage_known':True})+'\n')
    container = 'mistral-telemetry-'+uuid.uuid4().hex[:8]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    report = {'image':args.image,'version':args.version,'checks':[],'real_credentials_used':False}

    def http(path, body=None, method=None):
        key = 'telemetry-test-management' if '/management/' in path else 'telemetry-test-client'
        request = urllib.request.Request(f'http://127.0.0.1:{port}'+path, data=None if body is None else json.dumps(body).encode(), method=method, headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'})
        try:
            with opener.open(request,timeout=20) as response:
                return response.status,response.read()
        except urllib.error.HTTPError as error:
            return error.code,error.read()

    def api(path, body=None, method=None):
        status,raw = http('/v0/management/plugins/mistral-bridge'+path,body,method)
        assert status==200, (path,status,raw[:200])
        return json.loads(raw)

    def ready():
        for _ in range(100):
            try:
                status,raw=http('/v1/models')
                if status==200 and any(m['id']=='telemetry-test' for m in json.loads(raw)['data']):
                    return
            except (OSError,urllib.error.URLError):
                pass
            time.sleep(.2)
        raise AssertionError('CPA did not publish test models')

    def passed(name):
        report['checks'].append(name)
        print('PASS '+name,flush=True)

    def total():
        return api('/stats')['stats']['requests']

    def await_total(expected):
        for _ in range(50):
            if total()==expected:
                return
            time.sleep(.1)
        raise AssertionError(f'expected {expected}, received {total()}')

    def chat(stream=False, fail=False):
        status,raw=http('/v1/chat/completions',{'model':'telemetry-error' if fail else 'telemetry-test','messages':[{'role':'user','content':'Mock test'}],'max_tokens':16,'stream':stream})
        assert status==(400 if fail else 200),(status,raw[:200])
        if stream and not fail:
            assert b'[DONE]' in raw

    try:
        subprocess.run(['docker','run','-d','--name',container,'--network','host','-v',f'{auths}:/root/.cli-proxy-api','-v',f'{config_path}:/CLIProxyAPI/config.yaml',args.image],check=True,stdout=subprocess.DEVNULL)
        ready()
        initial=api('/stats')
        assert initial['stats']['requests']==10005 and initial['incomplete']
        passed('imports all 10005 legacy requests before retention')
        assert api('/state')['capabilities']['historical_stats']
        api('/telemetry/settings',{'max_entries':3,'max_days':30,'max_bytes':1048576},'PATCH')
        for streaming in [False,True,False,True]:
            chat(streaming)
        await_total(10009)
        chat(fail=True)
        await_total(10010)
        stats=api('/stats')['stats']
        assert stats['success']==10009 and stats['errors']==1
        assert stats['total_tokens']==10009*15 and stats['cache_samples']==4
        assert stats['cache_rate']==40
        timeline = api('/stats?timezone=Asia%2FSingapore')
        assert timeline['stats']['requests']==10010 and not timeline['timeline_partial']
        assert sum(point['total_tokens'] for point in timeline['series'])==stats['total_tokens']
        passed('local-date timeline persists beyond detail retention and matches cumulative totals')
        assert api('/stats?model=telemetry-test')['stats']['requests']==4
        assert api('/logs')['total']<=3 and (data/'requests.jsonl').stat().st_size<=1048576
        passed('normal, stream and failed executions update independent history while details are pruned')
        subprocess.run(['docker','restart',container],check=True,stdout=subprocess.DEVNULL)
        ready()
        assert total()==10010
        assert api('/telemetry/settings')['max_entries']==3
        passed('restart preserves history and retention settings without double counting')
        api('/logs',method='DELETE')
        assert api('/logs')['total']==0 and total()==10010
        passed('clearing details preserves historical statistics')
        api('/stats',method='DELETE')
        assert total()==0
        chat()
        await_total(1)
        subprocess.run(['docker','restart',container],check=True,stdout=subprocess.DEVNULL)
        ready()
        assert total()==1
        passed('reset opens a new statistics epoch and old requests stay excluded after restart')
        report['mock_requests']=len(calls)
        report['success']=True
    finally:
        logs=subprocess.run(['docker','logs',container],capture_output=True,text=True)
        (root/'cpa.log').write_text(logs.stdout+logs.stderr)
        subprocess.run(['docker','rm','-f',container],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        upstream.shutdown()
        (root/'report.json').write_text(json.dumps(report,indent=2)+'\n')


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--library',required=True)
    parser.add_argument('--version',required=True)
    parser.add_argument('--image',default='eceasy/cli-proxy-api:v8.0.78')
    parser.add_argument('--work-dir',required=True)
    run(parser.parse_args())
