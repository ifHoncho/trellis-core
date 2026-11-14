#!/usr/bin/env python3
"""Lab helper for the web UI tests (lab.sh W1-W4).

  web.py launch BIN SOCKET STATE_DIR
      Run webd the way trellis-webd.socket and .service do: bind ports 443
      and 80 here, then hand them to a webd that has no capabilities and
      sits in a network namespace of its own.
  web.py sandbox CMD...
      Run CMD with no capabilities (in this network namespace), to show
      what a process like webd can and cannot do.
  web.py get URL
      Print the status code and the start of the body.
  web.py probe HOST PORT
      "served" if a TLS handshake completes, "refused" otherwise.
  web.py admin BASE PASSWORD
      Sign in and exercise the API; prints key=value lines.
  web.py setup BASE PASSWORD PROFILE.yaml CALLSIGN
      Provision an unprovisioned node through its web API only.
"""
import http.client, json, os, socket, ssl, sys, urllib.parse

CAPLESS = ['setpriv', '--bounding-set', '-all', '--inh-caps', '-all', '--ambient-caps', '-all', '--no-new-privs']
CTX = ssl._create_unverified_context()  # the node's certificate is its own


def launch(binary, sock, state):
    fds = []
    for port in (443, 80):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.bind(('0.0.0.0', port))
        s.listen(64)
        fds.append(os.dup(s.fileno()))
    # systemd's protocol: the sockets are file descriptors 3 and up, and
    # LISTEN_PID names the process they are meant for. exec keeps the pid.
    high = [os.dup2(fd, 20 + i) for i, fd in enumerate(fds)]
    for i, fd in enumerate(high):
        os.dup2(fd, 3 + i)
        os.set_inheritable(3 + i, True)
    env = dict(os.environ, LISTEN_FDS=str(len(high)), LISTEN_PID=str(os.getpid()))
    cmd = ['unshare', '-n'] + CAPLESS + [binary, 'webd', '--socket', sock, '--state-dir', state, '--log-level', 'debug']
    os.execvpe(cmd[0], cmd, env)


def request(base, method, path, body=None, cookie=None, timeout=10):
    u = urllib.parse.urlparse(base)
    if u.scheme == 'https':
        conn = http.client.HTTPSConnection(u.hostname, u.port or 443, context=CTX, timeout=timeout)
    else:
        conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=timeout)
    headers = {}
    if method != 'GET':
        headers['X-Trellis-Request'] = '1'
    if body is not None:
        body = json.dumps(body)
        headers['Content-Type'] = 'application/json'
    if cookie:
        headers['Cookie'] = cookie
    conn.request(method, path, body=body, headers=headers)
    res = conn.getresponse()
    data = res.read().decode('utf-8', 'replace')
    set_cookie = res.getheader('Set-Cookie') or ''
    conn.close()
    try:
        doc = json.loads(data)
    except ValueError:
        doc = None
    return res.status, doc, data, set_cookie.split(';')[0], res.getheader('Location')


def out(key, value):
    print('%s=%s' % (key, value))


def admin(base, password):
    st, _, body, _, _ = request(base, 'GET', '/')
    out('index', '%d %s' % (st, 'trellis' if 'js/app.js' in body else 'other'))
    st, _, _, _, loc = request(base.replace('https://', 'http://'), 'GET', '/some/page')
    out('http', '%d %s' % (st, loc))
    out('noauth', request(base, 'GET', '/api/v1/status')[0])
    out('badlogin', request(base, 'POST', '/api/v1/login', {'password': password + 'x'})[0])
    st, _, _, cookie, _ = request(base, 'POST', '/api/v1/login', {'password': password})
    out('login', st)
    st, doc, _, _, _ = request(base, 'GET', '/api/v1/status', cookie=cookie)
    out('callsign', doc['node']['callsign'] if st == 200 else st)
    # A change through the UI's path: read the config, change one setting,
    # try it, apply it.
    st, cfg, _, _, _ = request(base, 'GET', '/api/v1/config', cookie=cookie)
    node = cfg['node']
    node['gateway']['down_mbit'] = 33
    st, dry, _, _, _ = request(base, 'PUT', '/api/v1/config/node?dry_run=1', node, cookie)
    out('dryrun', '%d changed=%s applied=%s' % (st, dry.get('changed'), dry.get('applied')))
    st, res, raw, _, _ = request(base, 'PUT', '/api/v1/config/node', node, cookie)
    out('apply', '%d applied=%s' % (st, res.get('applied') if res else raw[:120]))
    node['callsign'] = 'Not A Callsign'
    st, res, _, _, _ = request(base, 'PUT', '/api/v1/config/node', node, cookie)
    out('invalid', '%d %s' % (st, ','.join(f['field'] for f in (res or {}).get('fields', []))))
    out('nocsrf', request(base, 'GET', '/api/v1/web', cookie=cookie)[0])
    st, topo, _, _, _ = request(base, 'GET', '/api/v1/topology', cookie=cookie)
    if st == 200:
        both = [l for l in topo['links'] if l['ab_mbps'] > 0 and l['ba_mbps'] > 0]
        out('topology', 'nodes=%d reported=%d links=%d both=%d' % (len(topo['nodes']), sum(1 for n in topo['nodes'] if n['reported']), len(topo['links']), len(both)))
    else:
        out('topology', st)
    st, ex, _, _, _ = request(base, 'GET', '/api/v1/profile/export', cookie=cookie)
    out('export', '%d %s qr=%s' % (st, ex['code'][:5] if st == 200 else '', ex.get('qr', {}).get('size') if st == 200 else ''))
    request(base, 'POST', '/api/v1/logout', cookie=cookie)
    out('afterlogout', request(base, 'GET', '/api/v1/status', cookie=cookie)[0])


def setup(base, password, profile_path, callsign):
    st, s, _, _, _ = request(base, 'GET', '/api/v1/session')
    out('session', '%d setup=%s admin_set=%s' % (st, s.get('setup'), s.get('admin_set')))
    st, _, raw, cookie, _ = request(base, 'PUT', '/api/v1/password', {'new': password})
    out('password', st)
    profile = open(profile_path).read()
    st, dry, raw, _, _ = request(base, 'POST', '/api/v1/setup?dry_run=1', {'yaml': profile, 'callsign': callsign}, cookie)
    out('dryrun', '%d ssid=%s effects=%s' % (st, (dry or {}).get('ap', {}).get('ssid'), ','.join(e['code'] for e in (dry or {}).get('effects', []))) if st == 200 else '%d %s' % (st, raw[:200]))
    st, res, raw, _, _ = request(base, 'POST', '/api/v1/setup', {'yaml': profile, 'callsign': callsign}, cookie)
    out('apply', '%d applied=%s' % (st, (res or {}).get('applied')) if st == 200 else '%d %s' % (st, raw[:200]))
    print('cookie=' + cookie)


def main():
    cmd, a = sys.argv[1], sys.argv[2:]
    if cmd == 'launch':
        launch(*a)
    elif cmd == 'sandbox':
        os.execvp(CAPLESS[0], CAPLESS + a)
    elif cmd == 'get':
        st, _, body, _, loc = request(a[0], 'GET', urllib.parse.urlparse(a[0]).path or '/')
        print(st, loc or '', body[:160].replace('\n', ' '))
    elif cmd == 'probe':
        try:
            raw = socket.create_connection((a[0], int(a[1])), timeout=4)
            CTX.wrap_socket(raw, server_hostname=a[0]).close()
            print('served')
        except OSError as e:
            print('refused')
    elif cmd == 'admin':
        admin(*a)
    elif cmd == 'setup':
        setup(*a)
    elif cmd == 'put':
        # web.py put BASE COOKIE PATH JSON
        st, doc, raw, _, _ = request(a[0], 'PUT', a[2], json.loads(a[3]), a[1])
        print(st, raw[:300].replace('\n', ' '))
    elif cmd == 'config':
        st, doc, raw, _, _ = request(a[0], 'GET', '/api/v1/config', cookie=a[1])
        print(json.dumps(doc['node']) if st == 200 else st)
    else:
        sys.exit('unknown command ' + cmd)


if __name__ == '__main__':
    main()
