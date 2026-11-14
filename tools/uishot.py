#!/usr/bin/env python3
"""Screenshots of the web UI, for working on how it looks.

Drives a headless Firefox over its Marionette port: signs in, runs steps,
saves PNGs. Needs only Firefox and Python's standard library.

  make ui-dev                                   # in another terminal
  tools/uishot.py [options] step...

Options (each takes a value):
  --size WxH        window size (default 1440x900; Firefox goes no narrower than 500)
  --theme T         dusk, day or night
  --base URL        default http://127.0.0.1:8480
  --password PW     default trellis-demo; '' to stay signed out
  --out DIR         where the PNGs go (default .)

Steps, run in order:
  go:#/mesh         open a page of the UI
  wait:2.5          seconds
  shot:NAME         the window, to NAME.png
  full:NAME         the whole page, to NAME.png
  click:CSS         click the first element matching the selector
  type:CSS=>TEXT    type into it
  js:SCRIPT         run a script and print what it returns

JavaScript errors seen while the steps ran are printed at the end.

  tools/uishot.py --theme night go:#/mesh wait:5 shot:mesh-night
  tools/uishot.py --size 500x900 go:#/devices wait:2 full:phone-devices
"""
import base64, json, os, shutil, socket, subprocess, sys, tempfile, time

class FF:
    def __init__(self, port=2829):
        self.dir = tempfile.mkdtemp(prefix='ffprof-', dir=os.environ.get('FF_TMP'))
        with open(os.path.join(self.dir, 'user.js'), 'w') as f:
            f.write('user_pref("marionette.port", %d);\nuser_pref("browser.shell.checkDefaultBrowser", false);\n'
                    'user_pref("datareporting.policy.dataSubmissionEnabled", false);\nuser_pref("toolkit.telemetry.reportingpolicy.firstRun", false);\n'
                    'user_pref("browser.aboutwelcome.enabled", false);\nuser_pref("layout.css.devPixelsPerPx", "%s");\n' % (port, os.environ.get('FF_DPR', '1')))
        self.proc = subprocess.Popen(['firefox', '--headless', '--marionette', '--no-remote', '--profile', self.dir],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 40
        while True:
            try:
                self.sock = socket.create_connection(('127.0.0.1', port), timeout=2)
                break
            except OSError:
                if time.time() > deadline: raise
                time.sleep(0.3)
        self.sock.settimeout(60)
        self.buf = b''
        self.id = 0
        self._recv()
        self.cmd('WebDriver:NewSession', {'acceptInsecureCerts': True, 'capabilities': {'alwaysMatch': {'acceptInsecureCerts': True}}})
    def _recv(self):
        while b':' not in self.buf:
            self.buf += self.sock.recv(65536)
        n, rest = self.buf.split(b':', 1)
        n = int(n)
        while len(rest) < n:
            rest += self.sock.recv(1 << 20)
        self.buf = rest[n:]
        return json.loads(rest[:n])
    def cmd(self, name, params=None):
        self.id += 1
        msg = json.dumps([0, self.id, name, params or {}])
        self.sock.sendall(('%d:%s' % (len(msg), msg)).encode())
        while True:
            r = self._recv()
            if r[0] == 1 and r[1] == self.id:
                if r[2]:
                    raise RuntimeError('%s: %s' % (name, r[2].get('message')))
                return r[3]
    def js(self, script, *args):
        return self.cmd('WebDriver:ExecuteScript', {'script': script, 'args': list(args)}).get('value')
    def ajs(self, script, *args):
        return self.cmd('WebDriver:ExecuteAsyncScript', {'script': script, 'args': list(args)}).get('value')
    def shot(self, path, full=False):
        r = self.cmd('WebDriver:TakeScreenshot', {'full': full})
        with open(path, 'wb') as f:
            f.write(base64.b64decode(r['value']))
    def find(self, css):
        r = self.cmd('WebDriver:FindElement', {'using': 'css selector', 'value': css})
        return list(r['value'].values())[0]
    def close(self):
        try: self.cmd('Marionette:Quit', {'flags': ['eForceQuit']})
        except Exception: pass
        try: self.proc.wait(timeout=10)
        except Exception: self.proc.kill()
        shutil.rmtree(self.dir, ignore_errors=True)

def main():
    a = sys.argv[1:]
    if not a or a[0] in ('-h', '--help'):
        print(__doc__)
        return
    size, theme, base, pw, out = '1440x900', None, 'http://127.0.0.1:8480', 'trellis-demo', '.'
    while a and a[0].startswith('--'):
        k, v = a[0], a[1]; a = a[2:]
        if k == '--size': size = v
        elif k == '--theme': theme = v
        elif k == '--base': base = v
        elif k == '--password': pw = v
        elif k == '--out': out = v
    w, h = map(int, size.split('x'))
    ff = FF()
    try:
        ff.cmd('WebDriver:SetWindowRect', {'width': w, 'height': h})
        ff.cmd('WebDriver:Navigate', {'url': base + '/'})
        if theme:
            ff.js("localStorage.setItem('trellis.theme', arguments[0])", theme)
        if pw:
            ok = ff.ajs("const done = arguments[arguments.length-1]; fetch('/api/v1/login', {method:'POST', headers:{'X-Trellis-Request':'1','Content-Type':'application/json'}, body: JSON.stringify({password: arguments[0]})}).then(r => done(r.status))", pw)
            if ok != 200: print('login status', ok)
        ff.cmd('WebDriver:Navigate', {'url': base + '/'})
        time.sleep(1.2)
        ff.js("window.__errs = []; window.addEventListener('error', e => window.__errs.push(String(e.message) + ' @' + (e.filename||'').split('/').pop() + ':' + e.lineno)); window.addEventListener('unhandledrejection', e => window.__errs.push('rejection: ' + (e.reason && e.reason.stack || e.reason)));")
        for step in a:
            kind, _, arg = step.partition(':')
            if kind == 'go':
                ff.js("location.hash = arguments[0]", arg); time.sleep(0.9)
            elif kind == 'wait': time.sleep(float(arg))
            elif kind == 'shot': ff.shot(os.path.join(out, arg + '.png'))
            elif kind == 'full': ff.shot(os.path.join(out, arg + '.png'), full=True)
            elif kind == 'js': print(json.dumps(ff.js(arg)))
            elif kind == 'click': ff.cmd('WebDriver:ElementClick', {'id': ff.find(arg)}); time.sleep(0.5)
            elif kind == 'type':
                css, _, text = arg.partition('=>')
                ff.cmd('WebDriver:ElementSendKeys', {'id': ff.find(css), 'text': text}); time.sleep(0.2)
        print('errors', json.dumps(ff.js('return window.__errs || []')))
        vp = ff.js("return [innerWidth, innerHeight, devicePixelRatio]")
        print('viewport', vp)
    finally:
        ff.close()
main()
