#!/usr/bin/env python3
"""Record docs/live.gif: two people working on one list at the same time.

    python tools/screenshots/livegif.py

Alex is on a laptop and Sam on a phone, each in a headless Chrome of their
own (one Chrome would share its cookies, and so its sign-in, between them).
Sam adds a task and it appears on Alex's screen; Alex ticks one off and it
goes on Sam's; Sam comments and Alex cannot miss it.

It is a storyboard rather than a screen recording: each frame is taken when
the page has reached the state it shows, and held for as long as the story
needs. So the GIF is the same on a fast machine and a slow one, and never
catches a page half drawn. The changes that arrive from the other person
really do arrive through the app's live updates; nothing is reloaded.

Needs what shoot.py needs, plus Pillow: python -m pip install pillow
"""

import asyncio
import base64
import io
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from shoot import API, CDP, DOCS, REPO, find_chrome, free_port, kill_tree, page_target, seed, wait_for  # noqa: E402

try:
    import websockets
    from PIL import Image, ImageDraw, ImageFont
except ImportError:
    sys.exit("this needs websockets and Pillow: python -m pip install websockets pillow")

OUT = DOCS / "live.gif"
LAPTOP = (1024, 800)
PHONE = (390, 800)
SCALE = 0.8   # of the finished frame, to keep the file small
GAP = 28      # around and between the two screens
LABEL = 34    # the names above them
CAPTION = 58  # what is happening, underneath
BG = (233, 236, 242)
INK = (29, 39, 59)

# Every click shows where it landed, as a finger or a mouse would.
TAPS = """
addEventListener('DOMContentLoaded', () => {
  const s = document.createElement('style');
  s.textContent = '.__tap{position:fixed;width:38px;height:38px;margin:-19px 0 0 -19px;border-radius:50%;' +
    'background:rgba(32,107,196,.28);border:2px solid rgba(32,107,196,.8);pointer-events:none;z-index:99999}';
  document.head.appendChild(s);
});
addEventListener('pointerdown', e => {
  const d = document.createElement('div');
  d.className = '__tap';
  d.style.left = e.clientX + 'px';
  d.style.top = e.clientY + 'px';
  document.documentElement.appendChild(d);
  setTimeout(() => d.remove(), 700);
}, true);
"""

# centre(rowText, selector): where to click on something in the task whose
# title contains rowText (or on the page itself when rowText is empty),
# after bringing it into view.
HELPERS = """
window.__find = (rowText, sel) => {
  const scope = rowText ? [...document.querySelectorAll('.task-row')].find(r => r.querySelector('.task-title').textContent.includes(rowText)) : document;
  return scope && scope.querySelector(sel);
};
// Scroll only when something is not comfortably on screen (clear of the
// phone's tab bar), and instantly: Bootstrap scrolls smoothly, and positions
// must be read after the page has stopped moving.
window.__show = (el, block) => {
  const r = el.getBoundingClientRect();
  if (r.top < 70 || r.bottom > innerHeight - 90) el.scrollIntoView({block: block || 'center', behavior: 'instant'});
};
window.__centre = (rowText, sel) => {
  const el = __find(rowText, sel);
  if (!el) return null;
  __show(el);
  const r = el.getBoundingClientRect();
  return [r.left + r.width / 2, r.top + r.height / 2];
};
"""


def font(size, bold=False):
    names = ["segoeuib.ttf" if bold else "segoeui.ttf", "DejaVuSans-Bold.ttf" if bold else "DejaVuSans.ttf",
             "Arial Bold.ttf" if bold else "Arial.ttf"]
    for name in names:
        try:
            return ImageFont.truetype(name, size)
        except OSError:
            pass
    return ImageFont.load_default(size=size)


class Screen:
    """One person's browser."""

    def __init__(self, name, device, cdp, size, phone):
        self.name, self.device, self.cdp, self.size, self.phone = name, device, cdp, size, phone
        self.last = None

    async def open(self, base, token, path):
        c = self.cdp
        await c.send("Page.enable")
        await c.send("Network.enable")
        await c.send("Network.setCookie", name="doables_token", value=token, domain="localhost", path="/", httpOnly=True)
        w, h = self.size
        await c.send("Emulation.setDeviceMetricsOverride", width=w, height=h, deviceScaleFactor=1,
                     mobile=self.phone, screenWidth=w, screenHeight=h)
        await c.send("Emulation.setEmulatedMedia", media="screen",
                     features=[{"name": "prefers-color-scheme", "value": "light"}])
        await c.send("Page.addScriptToEvaluateOnNewDocument", source=TAPS)
        await c.send("Page.navigate", url=base + path)
        await c.wait_for("Page.loadEventFired")
        await self.js(HELPERS)
        await asyncio.sleep(1.5)  # fonts

    async def js(self, expr):
        r = await self.cdp.send("Runtime.evaluate", expression=expr, awaitPromise=True, returnByValue=True)
        if "exceptionDetails" in r:
            sys.exit(f"{self.name}: {expr[:60]}...: {r['exceptionDetails']}")
        return r.get("result", {}).get("value")

    async def until(self, expr, what, timeout=8):
        for _ in range(int(timeout / 0.05)):
            if await self.js(expr):
                return
            await asyncio.sleep(0.05)
        state = await self.js("JSON.stringify({url: location.href, focus: document.activeElement && (document.activeElement.name || document.activeElement.className), "
                              "rows: [...document.querySelectorAll('.task-row')].map(r => r.innerText.replace(/\s+/g, ' ').slice(0, 140))})")
        sys.exit(f"{self.name}'s screen never showed {what}; it shows {state}")

    async def click(self, row, sel):
        at = await self.js(f"__centre({json.dumps(row)}, {json.dumps(sel)})")
        if not at:
            sys.exit(f"{self.name}: nothing to click at {row!r} {sel!r}")
        await asyncio.sleep(0.15)  # let a scroll settle
        x, y = at
        for kind in ("mousePressed", "mouseReleased"):
            await self.cdp.send("Input.dispatchMouseEvent", type=kind, x=x, y=y, button="left", clickCount=1)
        # Then out of the way, so whatever ends up under it is not left
        # looking hovered, which on a phone would never happen.
        await self.cdp.send("Input.dispatchMouseEvent", type="mouseMoved", x=0, y=0)

    async def reveal(self, row):
        """Bring a task into view, if it is not already."""
        await self.js(f"__show(__find({json.dumps(row)}, '.task-title').closest('.task-row'))")

    async def type(self, text, per_frame=3, film=None):
        for i in range(0, len(text), per_frame):
            await self.cdp.send("Input.insertText", text=text[i:i + per_frame])
            if film:
                await film.frame(90)

    async def scroll_to(self, sel):
        await self.js(f"document.querySelector({json.dumps(sel)}).scrollIntoView({{block: 'start', behavior: 'instant'}}); "
                      "window.scrollBy({top: -12, behavior: 'instant'})")

    async def shoot(self):
        shot = await self.cdp.send("Page.captureScreenshot", format="png")
        self.last = Image.open(io.BytesIO(base64.b64decode(shot["data"]))).convert("RGB")
        return self.last


class Film:
    """Frames of the two screens side by side, with a caption."""

    def __init__(self, left, right):
        self.left, self.right = left, right
        self.caption = ""
        self.frames, self.durations = [], []
        self.label_font, self.caption_font = font(19, bold=True), font(23)

    async def frame(self, ms):
        if ms >= 1000:
            # A tap's ring belongs to the moment of the tap, not to the long
            # look at what it did.
            for screen in (self.left, self.right):
                await screen.js("document.querySelectorAll('.__tap').forEach(t => t.remove())")
        a, b = await self.left.shoot(), await self.right.shoot()
        w = GAP * 3 + a.width + b.width
        h = LABEL + GAP + max(a.height, b.height) + CAPTION
        img = Image.new("RGB", (w, h), BG)
        d = ImageDraw.Draw(img)
        x = GAP
        for screen, shot in ((self.left, a), (self.right, b)):
            d.text((x, LABEL - 10), f"{screen.name} · {screen.device}", font=self.label_font, fill=INK, anchor="ls")
            img.paste(shot, (x, LABEL))
            d.rectangle([x - 1, LABEL - 1, x + shot.width, LABEL + shot.height], outline=(200, 205, 214))
            x += shot.width + GAP
        d.text((w / 2, h - CAPTION / 2 + 2), self.caption, font=self.caption_font, fill=INK, anchor="mm")
        self.frames.append(img.resize((round(w * SCALE), round(h * SCALE)), Image.LANCZOS))
        self.durations.append(ms)

    def save(self, path):
        # One palette for the whole film, so colours do not flicker between frames.
        palette = self.frames[0].quantize(colors=255, method=Image.Quantize.MEDIANCUT)
        frames = [f.quantize(palette=palette, dither=Image.Dither.NONE) for f in self.frames]
        frames[0].save(path, save_all=True, append_images=frames[1:], duration=self.durations,
                       loop=0, optimize=True, disposal=1)


async def record(ports, base, tokens):
    async with websockets.connect(page_target(ports[0]), max_size=64 << 20) as ws_a, \
            websockets.connect(page_target(ports[1]), max_size=64 << 20) as ws_s:
        alex = Screen("Alex", "laptop", CDP(ws_a), LAPTOP, False)
        sam = Screen("Sam", "phone", CDP(ws_s), PHONE, True)
        await alex.open(base, tokens["Alex"], "/lists/1")
        await sam.open(base, tokens["Sam"], "/lists/1")
        await alex.scroll_to('[data-live="tasks"]')
        await sam.scroll_to("#add-task-form")
        film = Film(alex, sam)

        film.caption = "Alex and Sam share a list: Alex on a laptop, Sam on a phone."
        await film.frame(2600)

        # Sam adds a task; it turns up on Alex's screen by itself.
        film.caption = "Sam adds a task…"
        await sam.click("", "#add-task-form input[name=title]")
        await film.frame(300)
        await sam.type("Buy sunscreen #packing", film=film)
        await sam.click("", "#add-task-form button[type=submit]")
        await film.frame(250)
        await sam.until("!!__find('sunscreen', '.task-title')", "Sam's new task")
        await alex.until("!!__find('sunscreen', '.task-title')", "Sam's new task")
        await sam.reveal("sunscreen")
        await alex.reveal("sunscreen")
        film.caption = "…and it is on Alex's screen at once. Nobody reloads anything."
        await film.frame(2800)

        # Alex ticks one off; Sam sees it go.
        film.caption = "Alex ticks one off…"
        await alex.click("ferry", ".task-check-wrap")
        await film.frame(300)
        await alex.until("__find('ferry', '.task-check').checked", "the ferry ticked")
        await sam.until("__find('ferry', '.task-check').checked", "the ferry ticked")
        await sam.reveal("ferry")
        film.caption = "…and Sam sees it done, and who did it."
        await film.frame(2600)

        # Sam comments; Alex can't miss it.
        film.caption = "Sam has a question about dinner…"
        await sam.click("dinner", ".task-more")
        await film.frame(700)
        await sam.click("dinner", "[data-open-thread]")
        await film.frame(300)
        await sam.type("Found one by the water. 8pm?", film=film)
        await sam.click("dinner", "details[data-thread] button[type=submit]")
        await film.frame(250)
        await sam.until("!!__find('dinner', '.comment')", "his own comment")
        await alex.until("!!__find('dinner', '.unread-line')", "Sam's comment")
        film.caption = "…and Alex can't miss it: new comments stand out until they are read."
        await film.frame(3000)

        film.caption = "One click, and the conversation is open."
        await alex.click("dinner", "summary.unread-line")
        await film.frame(300)
        await alex.until("__find('dinner', 'details[data-thread]').open", "the thread open")
        await asyncio.sleep(0.3)
        await film.frame(3200)

        film.caption = "Doables: shared to-do lists you finish together, on your own server."
        await film.frame(3000)
        return film


def main():
    chrome = find_chrome()
    port = free_port()
    ports = (free_port(), free_port())
    base = f"http://localhost:{port}"
    work = Path(tempfile.mkdtemp(prefix="doables-gif-"))
    procs = []
    try:
        print(f"starting a server on {port}")
        procs.append(subprocess.Popen(
            ["go", "run", "./cmd/server", "-addr", f"localhost:{port}", "-db", str(work / "demo.db")],
            cwd=REPO, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        wait_for(base + "/welcome", "the server")

        print("seeding the demo data")
        api = API(base)
        tokens = seed(api)
        # A shorter list, so everything happens where the phone can show it,
        # and nothing already waiting to be read, so only the new comment is.
        api("/api/tasks/1", token=tokens["Alex"], method="DELETE")
        api("/api/tasks/4", token=tokens["Alex"], method="DELETE")
        for name in ("Alex", "Sam"):
            api.form("/tasks/2/seen", tokens[name])

        print("starting two Chromes")
        for i, dp in enumerate(ports):
            procs.append(subprocess.Popen(
                [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
                 "--hide-scrollbars", f"--remote-debugging-port={dp}",
                 f"--user-data-dir={work / f'profile{i}'}", "about:blank"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
            wait_for(f"http://127.0.0.1:{dp}/json/version", "Chrome")

        print("recording")
        film = asyncio.run(record(ports, base, tokens))
        film.save(OUT)
        seconds = sum(film.durations) / 1000
        print(f"wrote {OUT}: {len(film.frames)} frames, {seconds:.0f}s, "
              f"{film.frames[0].width}x{film.frames[0].height}, {OUT.stat().st_size // 1024} KB")
    finally:
        for p in reversed(procs):
            kill_tree(p)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
