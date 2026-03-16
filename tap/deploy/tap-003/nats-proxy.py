#!/usr/bin/env python3
"""TCP proxy: localhost:14222 -> 100.93.14.103:4222 via second Tailscale instance.
Uses tailscale nc through the skylens Tailscale socket.

Resilient version: detects dead tailscale nc subprocess and forcefully
tears down the client socket so async_nats sees a disconnect and reconnects."""

import socket
import subprocess
import threading
import sys
import os
import signal
import time

LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = 14222
TAILSCALE_SOCKET = "/var/run/tailscale-skylens/tailscaled.sock"
TARGET_HOST = "100.93.14.103"
TARGET_PORT = "4222"

def pipe_to_socket(proc_fd, sock, name="out"):
    """Forward subprocess stdout fd -> TCP socket."""
    try:
        while True:
            data = os.read(proc_fd, 65536)
            if not data:
                break
            sock.sendall(data)
    except OSError:
        pass
    finally:
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            sock.close()
        except OSError:
            pass

def socket_to_pipe(sock, proc_stdin, name="in"):
    """Forward TCP socket -> subprocess stdin."""
    try:
        while True:
            data = sock.recv(65536)
            if not data:
                break
            proc_stdin.write(data)
            proc_stdin.flush()
    except OSError:
        pass
    finally:
        try:
            proc_stdin.close()
        except OSError:
            pass

def handle_client(client_sock, addr):
    """Handle a single client connection by proxying through tailscale nc."""
    # Enable TCP keepalive to detect dead connections
    client_sock.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    # Start keepalive after 10s idle, probe every 5s, fail after 3 probes (25s total)
    client_sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPIDLE, 10)
    client_sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPINTVL, 5)
    client_sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPCNT, 3)

    print(f"Client connected: {addr}", flush=True)

    try:
        proc = subprocess.Popen(
            ["tailscale", "--socket", TAILSCALE_SOCKET, "nc", TARGET_HOST, TARGET_PORT],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
    except Exception as e:
        print(f"Failed to spawn tailscale nc: {e}", file=sys.stderr, flush=True)
        client_sock.close()
        return

    stdout_fd = proc.stdout.fileno()

    t_out = threading.Thread(target=pipe_to_socket, args=(stdout_fd, client_sock), daemon=True)
    t_in = threading.Thread(target=socket_to_pipe, args=(client_sock, proc.stdin), daemon=True)
    t_out.start()
    t_in.start()

    # Wait for tailscale nc to die — this is the key health check.
    # When it exits, forcefully tear down the client socket.
    proc.wait()
    exit_code = proc.returncode
    print(f"tailscale nc exited (code={exit_code}) for client {addr}, tearing down connection", flush=True)

    # Forcefully close the client socket so the NATS client sees a disconnect
    try:
        client_sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    try:
        client_sock.close()
    except OSError:
        pass

    # Wait for pipe threads to finish (they should exit quickly now)
    t_out.join(timeout=2)
    t_in.join(timeout=2)

    print(f"Connection cleaned up for {addr}", flush=True)

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))

    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind((LISTEN_HOST, LISTEN_PORT))
    server.listen(16)
    print(f"NATS proxy listening on {LISTEN_HOST}:{LISTEN_PORT} -> {TARGET_HOST}:{TARGET_PORT}", flush=True)

    try:
        while True:
            client_sock, addr = server.accept()
            threading.Thread(target=handle_client, args=(client_sock, addr), daemon=True).start()
    except (KeyboardInterrupt, SystemExit):
        pass
    finally:
        server.close()

if __name__ == "__main__":
    main()
