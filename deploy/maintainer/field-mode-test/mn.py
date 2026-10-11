# A minimal Marionette client for driving headless Firefox in tests.
import json, os, socket, time

class M:
    def __init__(self, port=2828):
        for _ in range(60):
            try:
                self.s = socket.create_connection(("127.0.0.1", port)); break
            except OSError:
                time.sleep(0.5)
        self.buf = b""; self.id = 0
        self.read()
        self.cmd("WebDriver:NewSession", {"capabilities": {"alwaysMatch": {"acceptInsecureCerts": True}}})
    def read(self):
        while b":" not in self.buf:
            self.buf += self.s.recv(65536)
        n, rest = self.buf.split(b":", 1); n = int(n)
        while len(rest) < n:
            rest += self.s.recv(65536)
        self.buf = rest[n:]
        return json.loads(rest[:n])
    def cmd(self, name, params=None):
        self.id += 1
        msg = json.dumps([0, self.id, name, params or {}]).encode()
        self.s.sendall(str(len(msg)).encode() + b":" + msg)
        while True:
            r = self.read()
            if r[0] == 1 and r[1] == self.id:
                if r[2]:
                    raise RuntimeError(f"{name}: {r[2]}")
                return r[3]
    def go(self, url): return self.cmd("WebDriver:Navigate", {"url": url})
    def js(self, script, *args): return self.cmd("WebDriver:ExecuteScript", {"script": script, "args": list(args)}).get("value")
    def ajs(self, script, *args): return self.cmd("WebDriver:ExecuteAsyncScript", {"script": script, "args": list(args), "scriptTimeout": 30000}).get("value")
    def url(self): return self.cmd("WebDriver:GetCurrentURL")["value"]
    flag = None
    def offline(self, on):
        if on:
            open(self.flag, "w").close()
        elif os.path.exists(self.flag):
            os.remove(self.flag)
    def wait(self, script, timeout=20):
        end = time.time() + timeout
        while time.time() < end:
            v = self.js(script)
            if v:
                return v
            time.sleep(0.3)
        raise TimeoutError(script)
