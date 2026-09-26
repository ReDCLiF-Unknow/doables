#!/usr/bin/env python3
"""Regenerate the screenshots in docs/.

    python tools/screenshots/shoot.py

It starts a Doables server on a spare port with an empty database, fills it
with a plausible shared list, drives headless Chrome over the DevTools
protocol, and writes the PNGs into docs/. Nothing it touches outlives the run:
the database, the browser profile and both processes live in a temporary
directory that is deleted at the end.

Needs Go, Chrome (or set the CHROME environment variable) and the `websockets`
package: python -m pip install websockets

Why DevTools rather than Chrome's own --screenshot flag: that flag ignores
prefers-color-scheme, so the light screenshot comes out as a second copy of
the dark one, and on Windows it clamps the window to about 476px, so a phone
screenshot is silently rendered at the wrong width. Emulation.* controls both
exactly, and Network.setCookie signs us in without a login form.
"""

import asyncio
import base64
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import date, timedelta
from pathlib import Path

try:
    import websockets
except ImportError:
    sys.exit("this needs the websockets package: python -m pip install websockets")

REPO = Path(__file__).resolve().parents[2]
DOCS = REPO / "docs"

# name, path, width, height, colour scheme, phone
SHOTS = [
    ("list-dark.png", "/lists/1", 1280, 940, "dark", False),
    ("list-light.png", "/lists/1", 1280, 940, "light", False),
    ("today-dark.png", "/today", 1312, 800, "dark", False),
    ("mine-dark.png", "/mine", 1312, 660, "dark", False),
    ("mobile-dark.png", "/lists/1", 390, 844, "dark", True),
]

CHROMES = [
    os.environ.get("CHROME"),
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "google-chrome",
    "chromium",
]


def find_chrome():
    for c in CHROMES:
        if c and (Path(c).exists() or shutil.which(c)):
            return c
    sys.exit("no Chrome found; set CHROME to its path")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def kill_tree(proc):
    """Stop a process and its children. `go run` builds and then execs the
    server as a child, so killing only the parent would leave it running."""
    if proc.poll() is not None:
        return
    if os.name == "nt":
        subprocess.run(["taskkill", "/T", "/F", "/PID", str(proc.pid)],
                       capture_output=True)
    else:
        proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def wait_for(url, what, tries=60):
    for _ in range(tries):
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return
        except (urllib.error.URLError, ConnectionError, OSError):
            time.sleep(1)
    sys.exit(f"{what} never came up at {url}")


class API:
    def __init__(self, base):
        self.base = base

    def form(self, path, token):
        """POST to one of the HTML endpoints, which answer with a redirect
        rather than JSON."""
        req = urllib.request.Request(self.base + path, data=b"", method="POST")
        req.add_header("Authorization", "Bearer " + token)
        urllib.request.urlopen(req).read()

    def __call__(self, path, body=None, token=None, method=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if token:
            req.add_header("Authorization", "Bearer " + token)
        with urllib.request.urlopen(req) as r:
            return json.load(r) if r.length != 0 else None


def seed(api):
    """Fill an empty database with a shared list worth photographing, and
    return everyone's tokens by name (Alex owns the lists). Dates are relative, so "Yesterday", "Today" and
    "Tomorrow" are always right whenever this is run."""
    today = date.today()
    day = lambda n: (today + timedelta(days=n)).isoformat()

    people = {name: api("/api/users", {"name": name})["token"] for name in ("Alex", "Sam", "Rae")}
    alex, sam, rae = people["Alex"], people["Sam"], people["Rae"]

    # These three have had these lists for a while, so they are past the
    # "save your sign-in key" reminder a brand new account sees.
    for token in people.values():
        api.form("/me/token/saved", token)

    lists = {}
    for name in ("Weekend in Lisbon", "Flat move", "Reading"):
        lists[name] = api("/api/lists", {"name": name}, alex)
    trip = lists["Weekend in Lisbon"]
    for token in (sam, rae):
        api("/api/join/" + trip["invite_code"], {}, token)

    def task(list_id, author, title, description="", due="", assignee=None, done=False):
        t = api(f"/api/lists/{list_id}/tasks",
                {"title": title, "description": description, "due_date": due}, author)
        if assignee is not None:
            api(f"/api/tasks/{t['id']}", {"assignee_id": assignee}, alex, method="PATCH")
        if done:
            api(f"/api/tasks/{t['id']}", {"done": True}, author, method="PATCH")
        return t

    # Alex is 1, Sam is 2, Rae is 3: the order they were created in above.
    trip_id = trip["id"]
    task(trip_id, alex, "Sort out the airport transfer #transport", "Seven of us, two bags each", day(-1), 1)
    ferry = task(trip_id, sam, "Book the ferry to Cacilhas #transport", "Friday evening, before sunset", day(0), 2)
    api(f"/api/tasks/{ferry['id']}/comments", {"body": "Is the 7pm one still running in September?"}, rae)
    api(f"/api/tasks/{ferry['id']}/comments", {"body": "Checked: last one is 21:30"}, sam)
    task(trip_id, alex, "Find somewhere for dinner on Friday #food", "Somewhere near the water", day(1), 3)
    task(trip_id, rae, "Renew the travel card #transport", "", day(4), 3)
    task(trip_id, alex, "Pack the camera #packing", "And the spare battery", "", 1)
    task(trip_id, sam, "Buy travel adapters #packing", "", "", None, done=True)

    flat = lists["Flat move"]["id"]
    task(flat, alex, "Call the letting agent", "Ask about the deposit", day(0), 1)
    task(flat, alex, "Return the router", "", day(-1))
    task(flat, alex, "Book a van", "", day(3), 1)

    task(lists["Reading"]["id"], alex, "Finish the Go book", "Chapter 12 onwards", day(3), 1)
    return people


def page_target(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json") as r:
        for t in json.load(r):
            if t["type"] == "page":
                return t["webSocketDebuggerUrl"]
    sys.exit("Chrome exposed no page to drive")


class CDP:
    """Just enough of the DevTools protocol: send a command, await its reply."""

    def __init__(self, ws):
        self.ws, self.n = ws, 0

    async def send(self, method, **params):
        self.n += 1
        await self.ws.send(json.dumps({"id": self.n, "method": method, "params": params}))
        while True:
            msg = json.loads(await self.ws.recv())
            if msg.get("id") == self.n:
                if "error" in msg:
                    sys.exit(f"{method}: {msg['error']}")
                return msg.get("result", {})

    async def wait_for(self, event, timeout=30):
        while True:
            msg = json.loads(await asyncio.wait_for(self.ws.recv(), timeout))
            if msg.get("method") == event:
                return msg


async def capture(debug_port, base, token):
    async with websockets.connect(page_target(debug_port), max_size=64 * 1024 * 1024) as ws:
        cdp = CDP(ws)
        await cdp.send("Page.enable")
        await cdp.send("Network.enable")
        # Sign in without a login form. The app's cookie is HttpOnly, which
        # only stops page scripts from reading it, not DevTools from setting it.
        await cdp.send("Network.setCookie", name="doables_token", value=token,
                       domain="localhost", path="/", httpOnly=True)
        for name, path, w, h, scheme, phone in SHOTS:
            await cdp.send("Emulation.setDeviceMetricsOverride", width=w, height=h,
                           deviceScaleFactor=2, mobile=phone, screenWidth=w, screenHeight=h)
            await cdp.send("Emulation.setEmulatedMedia", media="screen",
                           features=[{"name": "prefers-color-scheme", "value": scheme}])
            await cdp.send("Emulation.setTouchEmulationEnabled", enabled=phone, maxTouchPoints=5)
            await cdp.send("Page.navigate", url=base + path)
            await cdp.wait_for("Page.loadEventFired")
            await asyncio.sleep(2.5)  # the icon webfont and Tabler's CSS come from a CDN
            shot = await cdp.send("Page.captureScreenshot", format="png", captureBeyondViewport=False)
            out = DOCS / name
            out.write_bytes(base64.b64decode(shot["data"]))
            print(f"  {name}  {w}x{h} @2x {scheme}{' phone' if phone else ''}")


def main():
    chrome = find_chrome()
    port, debug_port = free_port(), free_port()
    base = f"http://localhost:{port}"
    work = Path(tempfile.mkdtemp(prefix="doables-shots-"))
    server = browser = None
    try:
        print(f"starting a server on {port}")
        server = subprocess.Popen(
            ["go", "run", "./cmd/server", "-addr", f"localhost:{port}", "-db", str(work / "demo.db")],
            cwd=REPO, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for(base + "/welcome", "the server")

        print("seeding the demo data")
        token = seed(API(base))["Alex"]

        print("starting Chrome")
        browser = subprocess.Popen(
            [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
             "--hide-scrollbars", f"--remote-debugging-port={debug_port}",
             f"--user-data-dir={work / 'profile'}", "about:blank"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for(f"http://127.0.0.1:{debug_port}/json/version", "Chrome")

        print(f"writing to {DOCS}")
        asyncio.run(capture(debug_port, base, token))
        print("done")
    finally:
        for p in (browser, server):
            if p:
                kill_tree(p)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
