#!/usr/bin/env python3
"""Record assets/demo.gif from assets/demo.tape.

The tape names the beats; this recorder plays them against the real ccmorph and
the scripted stand-in (assets/demo/bin/claude), then renders what a terminal
would have shown. It needs no pseudo-terminal: the child runs over pipes and the
recorder synthesizes the tty's echo layer — the characters as they are typed,
and the "$ " prompt whenever the shell is waiting on an empty line. Everything
else on screen (the listing, the launch, the projected status line, the hop) is
the product's own output.

From the repo root:

    uv run --with pyte --with pillow python assets/demo/record.py

The tape keeps vhs's directive names (Set/Type/Sleep/Hide/Show). The recorder
reads the settings that shape the picture (Width, Height, FontSize, Padding,
Margin, MarginFill, BorderRadius, TypingSpeed, Framerate) and the beats; the
theme is Catppuccin Mocha, as the tape names. --story prints the screen after
each beat, so a re-recording can be checked without reading the GIF.
"""

import argparse
import codecs
import os
import queue
import shutil
import subprocess
import tempfile
import threading
import time
from pathlib import Path

import pyte
from PIL import Image, ImageDraw, ImageFont

REPO = Path(__file__).resolve().parent.parent.parent

# Catppuccin Mocha: the terminal, its bar, and the sixteen SGR colours.
BG = "#1e1e2e"
BAR_BG = "#181825"
CURSOR = "#6c7088"
TITLE = "#a5adcb"
DOTS = ("#ff5f57", "#febc2e", "#28c840")
COLORS = {
    "default": "#cdd6f4",
    "black": "#45475a", "red": "#f38ba8", "green": "#a6e3a1", "yellow": "#f9e2af",
    "blue": "#89b4fa", "magenta": "#f5c2e7", "cyan": "#94e2d5", "white": "#bac2de",
    "brightblack": "#585b70", "brightred": "#eba0ac", "brightgreen": "#94e2d5",
    "brightyellow": "#f5e0dc", "brightblue": "#74c7ec", "brightmagenta": "#f5bde6",
    "brightcyan": "#89dceb", "brightwhite": "#a6adc8",
}
FONT_PATHS = (
    "/System/Library/Fonts/Menlo.ttc",
    "/System/Library/Fonts/Supplemental/Menlo-Regular.ttc",
    "/usr/share/fonts/truetype/menlo/Menlo-Regular.ttf",
)

# The stand-in parks its cursor on the prompt with this sequence; seeing it is
# how the recorder knows a session is ready for the next typed line.
PARK = "\r\x1b[1A\x1b[2C"


def parse_duration(text):
    """A vhs duration: 40ms, 1s, 1.5s."""
    text = text.strip().lower()
    return float(text[:-2]) / 1000 if text.endswith("ms") else float(text[:-1])


def parse_tape(path):
    """One (settings, beats) pair. Beats are (kind, value) tuples."""
    settings = {"Shell": "bash", "Output": "assets/demo.gif", "FontSize": 17,
                "Width": 960, "Height": 560, "Padding": 24, "Margin": 20,
                "MarginFill": "#312e81", "BorderRadius": 10,
                "TypingSpeed": "40ms", "Framerate": 12}
    beats = []
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        cmd, _, rest = line.partition(" ")
        rest = rest.strip()
        if cmd == "Set":
            key, _, value = rest.partition(" ")
            settings[key] = value.strip('"')
        elif cmd == "Output":
            settings["Output"] = rest
        elif cmd in ("Hide", "Show"):
            beats.append((cmd, None))
        elif cmd == "Type":
            text, enter = rest, False
            if rest.startswith('"'):
                end = rest.index('"', 1)
                text, tail = rest[1:end], rest[end + 1:].strip()
                enter = tail in ("", "Enter")
            beats.append(("Type", (text, enter)))
        elif cmd == "Sleep":
            beats.append(("Sleep", parse_duration(rest)))
        else:
            raise SystemExit(f"assets/demo.tape: unknown directive {cmd!r}")
    return settings, beats


class Terminal:
    """The terminal's state (pyte) and the frames rendered from it."""

    def __init__(self, settings):
        self.width, self.height = int(settings["Width"]), int(settings["Height"])
        self.margin, self.padding = int(settings["Margin"]), int(settings["Padding"])
        self.margin_fill = settings["MarginFill"]
        self.radius = int(settings["BorderRadius"])
        self.typing = parse_duration(settings["TypingSpeed"])
        self.min_frame_gap = 1.0 / max(int(settings["Framerate"]), 1)
        self.font_size = int(settings["FontSize"])
        self.font = self.load_font(0)
        self.font_bold = self.load_font(1)
        self.advance = self.font.getlength("M")
        ascent, descent = self.font.getmetrics()
        self.line_height = ascent + descent
        self.bar_height = 40
        self.text_x = self.margin + self.padding
        self.text_y = self.margin + self.bar_height + self.padding
        self.cols = int((self.width - 2 * self.text_x) // self.advance)
        self.rows = int((self.height - self.text_y - self.margin - self.padding)
                        // self.line_height)
        self.frames = []       # (monotonic time, image)
        self.last_frame = 0.0
        self.recent = ""       # the last characters fed, for the park marker
        self.decoder = codecs.getincrementaldecoder("utf-8")("replace")
        self.reset()
        self.template = self.draw_window()

    def load_font(self, index):
        for path in FONT_PATHS:
            if os.path.exists(path):
                return ImageFont.truetype(path, self.font_size, index=index)
        raise SystemExit("record.py: no monospace font found (looked for Menlo)")

    def reset(self):
        """A fresh screen: what Show presents after the hidden setup."""
        self.screen = pyte.Screen(self.cols, self.rows)
        self.stream = pyte.Stream(self.screen)
        self.recent = ""

    # -- window chrome, drawn once --------------------------------------

    def draw_window(self):
        """The margin, bar and terminal area every frame is drawn onto."""
        img = Image.new("RGB", (self.width, self.height), self.margin_fill)
        d = ImageDraw.Draw(img)
        w, h, m, r, bar = (self.width, self.height, self.margin,
                           self.radius, self.bar_height)
        d.rounded_rectangle([m, m, w - m, h - m], radius=r, fill=BAR_BG)
        # The terminal area: rounded at the window's bottom, square under the bar.
        d.rounded_rectangle([m, m + bar - r, w - m, h - m], radius=r, fill=BG)
        d.rectangle([m, m + bar, w - m, m + bar + r], fill=BG)
        for i, color in enumerate(DOTS):
            x, y = m + 20 + i * 22, m + bar // 2
            d.ellipse([x - 6, y - 6, x + 6, y + 6], fill=color)
        title = ImageFont.truetype(self.font.path, 13)
        d.text((w // 2, m + bar // 2), "bash", font=title, fill=TITLE, anchor="mm")
        return img

    # -- feeding and frames ---------------------------------------------

    def feed(self, data):
        """Child bytes (or the recorder's own echo) become terminal state,
        the way a tty would show them: ONLCR turns each newline into CRLF."""
        if isinstance(data, str):
            text = data
        else:  # a pipe read may split a character; the decoder keeps it whole
            text = self.decoder.decode(data)
        text = text.replace("\r\n", "\n").replace("\n", "\r\n")
        self.stream.feed(text)
        self.recent = (self.recent + text)[-32:]

    def frame(self, force=False):
        now = time.monotonic()
        if not force and now - self.last_frame < self.min_frame_gap:
            return
        self.last_frame = now
        self.frames.append((now, self.render()))

    def render(self):
        img = self.template.copy()
        d = ImageDraw.Draw(img)
        s = self.screen
        for row in range(self.rows):
            y = self.text_y + row * self.line_height
            cells = s.buffer[row]
            col = 0
            while col < self.cols:
                cell = cells[col]
                if cell.data == " ":
                    col += 1
                    continue
                start = col
                while (col < self.cols and cells[col].data != " "
                       and cells[col].fg == cell.fg
                       and cells[col].bold == cell.bold):
                    col += 1
                text = "".join(cells[i].data for i in range(start, col))
                font = self.font_bold if cell.bold else self.font
                d.text((self.text_x + start * self.advance, y), text,
                       font=font, fill=COLORS.get(cell.fg, COLORS["default"]),
                       anchor="la")
        x = self.text_x + s.cursor.x * self.advance
        y = self.text_y + s.cursor.y * self.line_height
        d.rectangle([x, y, x + self.advance, y + self.line_height], fill=CURSOR)
        return img

    # -- the tty's echo layer -------------------------------------------

    def cursor_on_blank_line(self):
        s = self.screen
        if s.cursor.x != 0:
            return False
        return all(s.buffer[s.cursor.y][c].data == " " for c in range(self.cols))

    def type_line(self, text):
        if self.cursor_on_blank_line():
            self.feed("$ ")  # the shell's prompt, where a terminal would show it
            self.frame()
        for ch in text:
            self.feed(ch)
            self.frame()
            time.sleep(self.typing)
        self.feed("\r\n")
        self.frame(force=True)

    def story_dump(self, label):
        print(f"--- {label} ---")
        for line in self.screen.display:
            if line.strip():
                print(line.rstrip())


class Child:
    """The bash that plays the tape, with its output arriving on a queue."""

    def __init__(self, env, cwd):
        self.p = subprocess.Popen(["bash", "--noprofile", "--norc", "-s"],
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT, env=env, cwd=cwd)
        self.out = queue.Queue()
        threading.Thread(target=self._pump, daemon=True).start()

    def _pump(self):
        while True:
            data = os.read(self.p.stdout.fileno(), 65536)
            if not data:
                break
            self.out.put(data)
        self.out.put(None)

    def send(self, line):
        self.p.stdin.write((line + "\n").encode())
        self.p.stdin.flush()

    def alive(self):
        return self.p.poll() is None

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=30)
        except Exception:
            self.p.kill()


def wait_ready(term, child, require_clear=False, settle=1.2, cap=30.0):
    """Wait until the child can take a line: a session prompt parked its
    cursor, or the child fell silent. require_clear also waits out the
    setup's clear, so the Show that follows presents a finished screen —
    the setup's own output stays hidden either way."""
    started = last_output = time.monotonic()
    clear_seen = not require_clear
    while True:
        now = time.monotonic()
        if not child.alive():
            return ""
        try:
            data = child.out.get(timeout=0.05)
        except queue.Empty:
            data = None
        if data is not None:
            last_output = now
            if clear_seen:
                term.feed(data)
                term.frame()
            elif b"\x1b[2J" in data:
                clear_seen = True
        quiet = now - last_output
        if clear_seen and ((PARK in term.recent and quiet >= 0.15) or quiet >= settle):
            return ""
        if now - started > cap:
            return " (waited out the cap)"


def hold(term, child, seconds):
    """A Sleep beat: keep feeding output, keep the frames coming."""
    end = time.monotonic() + seconds
    while True:
        now = time.monotonic()
        if now >= end or not child.alive():
            return
        try:
            data = child.out.get(timeout=min(0.05, end - now))
        except queue.Empty:
            continue
        if data is None:
            return
        term.feed(data)
        term.frame()


def main():
    ap = argparse.ArgumentParser(description="Record the README demo GIF.")
    ap.add_argument("--story", action="store_true",
                    help="print the screen after each beat, for verification")
    args = ap.parse_args()

    settings, beats = parse_tape(REPO / "assets" / "demo.tape")
    term = Terminal(settings)
    home = tempfile.mkdtemp(prefix="ccmorph-demo-record.")
    env = {"HOME": home, "TERM": "xterm-256color", "PS1": "$ ",
           "PATH": f"{REPO}/assets/demo/bin:{REPO}/bin:/usr/bin:/bin:/usr/sbin:/sbin",
           "TMPDIR": os.environ.get("TMPDIR", "/tmp")}

    def settle(label):
        """Wait out whatever the last sent line is still saying."""
        note = wait_ready(term, child)
        if args.story and label is not None:
            term.story_dump(f"after {label!r}{note}")

    try:
        child = Child(env, REPO)
        hidden = False
        last_sent = None
        for kind, value in beats:
            if kind == "Hide":
                hidden = True
            elif kind == "Show":
                term.reset()
                term.frame(force=True)
                hidden = False
            elif kind == "Sleep":
                hold(term, child, value)
            elif kind == "Type":
                text, enter = value
                if hidden:
                    # The one hidden line is the setup: send it, then let it
                    # finish before the Show presents the screen it cleared.
                    child.send(text)
                    wait_ready(term, child, require_clear=True)
                    continue
                settle(last_sent)
                if enter:
                    term.type_line(text)
                child.send(text)
                last_sent = text
        hold(term, child, 1.5)  # let the exit tail land
        if args.story and last_sent is not None:
            term.story_dump(f"after {last_sent!r} (end)")
        child.close()
    finally:
        shutil.rmtree(home, ignore_errors=True)

    durations = []
    for i in range(len(term.frames) - 1):
        gap = (term.frames[i + 1][0] - term.frames[i][0]) * 1000
        durations.append(min(2500, max(50, gap)))
    durations.append(2000)
    images = [f[1] for f in term.frames]
    out = REPO / settings["Output"]
    images[0].save(out, save_all=True, append_images=images[1:],
                   duration=durations, loop=0)
    print(f"{out}: {len(images)} frames, {out.stat().st_size} bytes")


if __name__ == "__main__":
    main()
