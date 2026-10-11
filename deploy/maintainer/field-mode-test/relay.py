# A TCP relay that drops every connection while the "down" file exists,
# so a browser sees the server vanish as if the network were gone.
import os, socket, sys, threading
listen, target, flag = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
def pipe(a, b):
    try:
        while (d := a.recv(65536)):
            if os.path.exists(flag):
                break
            b.sendall(d)
    except OSError:
        pass
    for s in (a, b):
        try: s.close()
        except OSError: pass
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", listen)); srv.listen(64)
while True:
    c, _ = srv.accept()
    if os.path.exists(flag):
        c.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, b"\x01\x00\x00\x00\x00\x00\x00\x00"); c.close(); continue
    try:
        u = socket.create_connection(("127.0.0.1", target))
    except OSError:
        c.close(); continue
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
