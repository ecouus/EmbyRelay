"""Exercise a running deployment using temporary resources; no credentials logged.
RELAY_TEST_URL and RELAY_TEST_PASSWORD are required. For a Docker target set
RELAY_FIXTURE_HOST=host.docker.internal and RELAY_FIXTURE_CIDR to its resolved /32.
"""
import concurrent.futures
import http.cookiejar
import http.server
import json
import os
import threading
import urllib.request

base = os.environ['RELAY_TEST_URL'].rstrip('/')
password = os.environ['RELAY_TEST_PASSWORD']
payload = b'EmbyRelay fixture\n' * 4096

class Origin(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_GET(self):
        if self.path.startswith('/System/Info/Public'):
            body = b'{"ServerName":"Integration fixture"}'
            self.send_response(200)
        else:
            first, last = 0, len(payload) - 1
            if self.headers.get('Range'):
                first, last = map(int, self.headers['Range'].removeprefix('bytes=').split('-'))
                self.send_response(206)
                self.send_header('Content-Range', f'bytes {first}-{last}/{len(payload)}')
            else:
                self.send_response(200)
            body = payload[first:last + 1]
            self.send_header('ETag', '"fixture-v1"')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

origin = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Origin)
thread = threading.Thread(target=origin.serve_forever, daemon=True)
thread.start()
jar = http.cookiejar.CookieJar()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

def api(path, method='GET', body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + '/api/v1/' + path, data=data, method=method,
        headers={'Content-Type': 'application/json', 'X-Requested-With': 'EmbyRelay'})
    with client.open(request, timeout=10) as response:
        return json.load(response)

route_id = upstream_id = None
try:
    api('auth/login', 'POST', {'username': 'admin', 'password': password})
    host = os.environ.get('RELAY_FIXTURE_HOST', '127.0.0.1')
    cidr = os.environ.get('RELAY_FIXTURE_CIDR', '127.0.0.1/32')
    import secrets
    name = 'smoke-' + secrets.token_hex(6)
    api('upstreams', 'POST', {'name': name, 'base_url': f'http://{host}:{origin.server_port}',
        'enabled': True, 'mode': 'proxy', 'allowed_cidrs': [cidr], 'redirect_hosts': []})
    upstream_id = next(u['id'] for u in api('upstreams') if u['name'] == name)
    health = api(f'upstreams/{upstream_id}/health')
    assert health['healthy'], f'Origin health failed: {health}'
    api('routes', 'POST', {'alias': name, 'upstream_id': upstream_id, 'enabled': True,
        'relay_id': '', 'rate_bps': 0, 'daily_quota': 0, 'max_concurrent': 100})
    route_id = next(r['id'] for r in api('routes') if r['alias'] == name)
    key = api(f'routes/{route_id}/keys', 'POST', {'expires_at': 0})['access_key']
    playback = f'{base}/r/{name}/{key}/Videos/1/stream'
    def play(_):
        request = urllib.request.Request(playback, headers={'Range': 'bytes=0-65535'})
        with urllib.request.urlopen(request, timeout=30) as response:
            data = response.read()
            assert response.status == 206 and data == payload[:65536]
    with concurrent.futures.ThreadPoolExecutor(max_workers=100) as executor:
        list(executor.map(play, range(100)))
    traffic = next(t for t in api('traffic') if t['route_id'] == route_id)
    assert traffic['egress_bytes'] == 100 * 65536, traffic
    assert traffic['ingress_bytes'] == 100 * 65536, traffic
    assert len(api('sessions?limit=1000')) >= 100
    print('PASS: login, upstream health, route/key creation, 100 concurrent 206 responses, exact 6,553,600-byte ingress/egress totals, and sessions.')
finally:
    if route_id:
        api('routes/' + route_id, 'DELETE')
    if upstream_id:
        api('upstreams/' + upstream_id, 'DELETE')
    origin.shutdown()
    origin.server_close()
