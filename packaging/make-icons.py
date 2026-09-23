#!/usr/bin/env python3
"""Draws Ora's face as an icon and writes every size the repo ships.

The face is set in type, not drawn. The design sheets of 2026-09-12 show a kaomoji whose parentheses, hyphens and undertie are all glyphs a type designer already drew, with the optical weight and the round terminals that come with that; the two earlier versions of this script tried to reproduce them from arcs and rounded rectangles and read, in the user's words, like a coded SVG. Setting the real characters in JetBrains Mono gets that craft for nothing, and it makes the icon the same artefact as the face the window shows, which is also this text in the same family.

Two things the type needs before it looks set rather than dumped:
  tracking  JetBrains Mono is monospaced, so an untracked "(-‿-)" stands in five equal columns. Every glyph's advance is tightened by TRACKING so the parentheses hug the features, which is the spacing the sheets draw. Measured against the sheets at -22%; 0 reads as columns and -30% starts to crowd the eyes into the brackets.
  weight    an ExtraLight hairline is thinner than a pixel at 32px and vanishes, so the weight steps up as the target gets smaller (see WEIGHT_FOR).

Two treatments, because the two places an icon goes want opposite things:
  app   a superellipse tile in the palest lavender, top to bottom, with the face in dark ink, which is what the sheets show and what a launcher grid wants
  tray  the bare face on transparency in one mid-violet, because a shell top bar may be light or dark and an opaque tile of either colour is wrong on one of them

Run: python3 packaging/make-icons.py
Needs: Pillow and numpy, and JetBrains Mono installed (FONT_PATH).
"""

import sys
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw, ImageFont

# The face itself. (-‿-) is the one the sheets use everywhere the icon is not making a point: the app tile, the tray at idle, the notification tiles, the input bar, the menu bar.
FACE = "(-‿-)"

# Ink for the app icon: near-black with a violet cast, so it sits with the window's own text rather than looking like a second brand.
APP_INK = (28, 25, 45, 255)
# The app tile, top colour to bottom colour: the palest lavender fading to almost white, which is the soft vertical wash the sheets draw rather than a flat fill.
APP_GROUND = ((238, 236, 252), (252, 252, 254))
# Ink for the tray, where there is no tile: mid-violet, which keeps its contrast on both a near-white and a near-black shell.
TRAY_INK = (101, 75, 251, 255)

FONT_PATH = str(Path.home() / ".local/share/fonts/JetBrainsMono/JetBrainsMonoNerdFont-%s.ttf")

# How much of each glyph's advance to take back. See the tracking note above.
TRACKING = -0.22

# The face's width as a fraction of the tile's, which is how the sheets place it: a compact unit in the middle with real margin, not art filling the square.
FACE_WIDTH = 0.60

# The tray has no tile to leave a margin inside, so it is set nearly edge to edge, and in Bold, which is what a glyph four pixels wide needs to hold its shape. Measured at 16, 22, 24 and 32px against Medium: Medium is the better weight once the icon is 48px or more, and at panel height its stems fall under a pixel and grey out.
TRAY_FACE_WIDTH = 0.96
TRAY_WEIGHT = "Bold"

# The sizes the tray master is written at. StatusNotifierItem's IconPixmap is an array — a(iiay) — and the host picks the closest to its panel height, so handing it the exact sizes means it never resamples. One 128px pixmap was all it had before, and every panel in the world scaled that down to 22, which is where the blur came from. 44 and 48 are the same panel heights at 2x for a HiDPI shell.
TRAY_SIZES = [16, 22, 24, 32, 44, 48]

# The superellipse exponent for the tile. A rounded rectangle joins its straight edge to its corner arc at a visible kink; |x|^n + |y|^n = 1 does not, which is the shape a launcher grid and the sheets both draw.
SQUIRCLE_N = 4.2

# Where each PNG goes and which treatment it gets. The tray is not here: its sizes are written into cmd/tray/, which cmd/tray_linux.go embeds whole. app/src-tauri/icons/icon.png is what packaging/release.sh resizes the installed hicolor icons from, and the three beside it are the ones tauri.conf.json names.
TARGETS = {
    "cmd/app_icon_linux.png": (512, "app"),
    "app/src/assets/ora.png": (128, "app"),
    "app/src-tauri/icons/icon.png": (512, "app"),
    "app/src-tauri/icons/128x128.png": (128, "app"),
    "app/src-tauri/icons/128x128@2x.png": (256, "app"),
    "app/src-tauri/icons/64x64.png": (64, "app"),
    "app/src-tauri/icons/32x32.png": (32, "app"),
}

# The .ico file carries the sizes Windows picks between for a title bar.
ICO_TARGETS = {"app/src-tauri/icons/icon.ico": "app"}
ICO_SIZES = [16, 32, 48, 128, 256]

# Drawn four times up and scaled down, which is how the glyph edges and the tile's corners come out smooth without any blur pass of their own.
# Only the app tile gets it. A tray icon is drawn at its final size and never resampled: FreeType hints the stems onto the pixel grid at the size it is asked for, and supersampling throws that alignment away — it is the difference between crisp and washed at 22px.
SUPERSAMPLE = 4


def weight_for(size: int, treatment: str) -> str:
    """Which cut of JetBrains Mono to set at. Input: the finished icon's side in pixels and "app" or "tray". Output: the font's weight name.
    A hairline that is under a pixel wide disappears on the downsample, so the smaller the icon the heavier the cut it is set in. The tray always takes TRAY_WEIGHT, because its 128px master says nothing about the size it is drawn at: the shell shrinks it to panel height whatever this file writes.
    """
    if treatment == "tray":
        return TRAY_WEIGHT
    if size >= 96:
        return "ExtraLight"
    if size >= 48:
        return "Light"
    return "Regular"


def squircle_mask(size: int) -> Image.Image:
    """The tile's shape as an alpha mask. Input: the side in pixels. Output: an L-mode image, opaque inside the superellipse."""
    t = np.linspace(-1, 1, size)
    x, y = np.meshgrid(t, t)
    inside = 1.0 - (np.abs(x) ** SQUIRCLE_N + np.abs(y) ** SQUIRCLE_N)
    # Scaled so the transition is about a pixel wide, which the supersampled downsample then turns into a clean edge.
    return Image.fromarray((np.clip(inside * size / 3.0 + 0.5, 0, 1) * 255).astype(np.uint8), "L")


def ground(size: int, top, bottom) -> Image.Image:
    """The tile's vertical wash. Input: the side in pixels and the top and bottom colours. Output: an RGB image."""
    down = np.linspace(0, 1, size)[:, None]
    arr = np.zeros((size, size, 3))
    for i in range(3):
        arr[:, :, i] = top[i] * (1 - down) + bottom[i] * down
    return Image.fromarray(arr.astype(np.uint8), "RGB")


def tracked_width(text: str, font: ImageFont.FreeTypeFont) -> float:
    """How wide the text is once tracked. Input: the text and the font. Output: the width in pixels.
    The last glyph contributes its own advance untightened, since nothing follows it to close up against.
    """
    return sum(font.getlength(c) * (1 + TRACKING) for c in text[:-1]) + font.getlength(text[-1])


def fit_font(text: str, side: int, weight: str, width: float) -> ImageFont.FreeTypeFont:
    """The largest size of the font at which the tracked text still fits the given fraction of the tile. Input: the text, the tile's side in pixels, the weight name, and the fraction of the side the text may take. Output: the font."""
    px = side
    font = ImageFont.truetype(FONT_PATH % weight, px)
    while tracked_width(text, font) > side * width and px > 8:
        px = int(px * 0.94)
        font = ImageFont.truetype(FONT_PATH % weight, px)
    return font


def draw_tracked(d: ImageDraw.ImageDraw, x: float, y: float, text: str, font: ImageFont.FreeTypeFont, fill) -> None:
    """Draws the text one glyph at a time with each advance tightened by TRACKING. Input: the draw, the top-left of the text, the text, the font and the ink. Output: none; it draws."""
    for ch in text:
        d.text((x, y), ch, font=font, fill=fill)
        x += font.getlength(ch) * (1 + TRACKING)


def draw_face(size: int, treatment: str, face: str = FACE) -> Image.Image:
    """Draws one icon. Input: the square's side in pixels, "app" or "tray", and the face to set. Output: the image, RGBA.
    The app tile is drawn SUPERSAMPLE times up and scaled down; the tray is drawn once at the size asked for, with the text placed on whole pixels, so nothing ever resamples it.
    """
    up = SUPERSAMPLE if treatment == "app" else 1
    s = size * up
    img = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    if treatment == "app":
        tile = ground(s, *APP_GROUND).convert("RGBA")
        tile.putalpha(squircle_mask(s))
        img.alpha_composite(tile)

    font = fit_font(face, s, weight_for(size, treatment), FACE_WIDTH if treatment == "app" else TRAY_FACE_WIDTH)
    box = font.getbbox(face)
    # Centred on the ink, not on the em box, and lifted a hair: the undertie sits low in the glyph, so centring the em box leaves the face looking as though it has slipped.
    x = (s - tracked_width(face, font)) / 2
    y = (s - (box[3] - box[1])) / 2 - box[1] - s * 0.01
    if up == 1:
        # Whole pixels: a glyph started at x.5 is rendered across two pixel columns at half weight each, which is the same grey the downsample used to give.
        x, y = round(x), round(y)
    draw_tracked(ImageDraw.Draw(img), x, y, face, font, APP_INK if treatment == "app" else TRAY_INK)
    return img if up == 1 else img.resize((size, size), Image.LANCZOS)


def main() -> int:
    root = Path(__file__).resolve().parent.parent
    # The tray's own sizes, each drawn at its own size. cmd/tray_linux.go embeds the directory and offers every one of them as a pixmap.
    tray_dir = root / "cmd" / "tray"
    tray_dir.mkdir(exist_ok=True)
    for size in TRAY_SIZES:
        draw_face(size, "tray").save(tray_dir / f"{size}.png")
        print(f"wrote cmd/tray/{size}.png at {size}px, tray")

    for relative, (size, treatment) in TARGETS.items():
        path = root / relative
        if not path.parent.is_dir():
            print(f"no such directory: {path.parent}", file=sys.stderr)
            return 1
        draw_face(size, treatment).save(path)
        print(f"wrote {relative} at {size}px, {treatment}")

    # Each .ico is written from one large source, since Pillow builds every size in it from the image it is handed.
    for relative, treatment in ICO_TARGETS.items():
        path = root / relative
        draw_face(max(ICO_SIZES), treatment).save(path, sizes=[(n, n) for n in ICO_SIZES])
        print(f"wrote {relative} at {', '.join(str(n) for n in ICO_SIZES)}, {treatment}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
