#!/usr/bin/env python3
"""
gen_info_card.py — editorial info-card renderer for briefing-v3.

Produces two kinds of 1600x1600 PNG "info cards" on a newspaper-style
off-white background, inspired by the ai.hubtoday.app layout but
independently implemented:

  --mode item    one card per news item (main body of the briefing)
  --mode header  one card per issue (the "大字报" banner on top)

Input is a JSON payload on stdin describing the card. The JSON shape
is intentionally the same shape the Go infocard package produces so
the Go orchestrator can pipe straight through.

Example item JSON:

    {
      "main_title": "Anthropic Claude Sonnet 4.6",
      "subtitle": "一天连发编码 Agent + 托管基础设施",
      "intro": "Anthropic 在同一天宣布 Sonnet 4.6 ...",
      "hero_number": "4.6",
      "hero_label": "新版本号",
      "stat_numbers": [
        {"value": "$0.08/h", "label": "Agent 托管定价"},
        {"value": "3x", "label": "编码速度"}
      ],
      "key_points": [
        {"title": "编码", "desc": "智能体式编码能力"},
        {"title": "智能体", "desc": "自主执行多步任务"},
        {"title": "专业场景", "desc": "金融、法律、医疗"}
      ],
      "footer_summary": "头部公司已不止卖模型, 开始卖完整 Agent 环境",
      "brand_tag": "产品与功能更新",
      "category_tag": "MODEL"
    }

Example header JSON:

    {
      "issue_date": "2026-04-10",
      "main_headline": "Anthropic 重磅 · Claude 4.6 与 Agent 同日连发",
      "sub_headline": "OpenAI 下放安全刹车，Meta 改走闭源路线",
      "top_stories": [
        {"title": "Claude Sonnet/Opus 4.6 一天双发", "tag": "MODEL"},
        {"title": "Anthropic 托管 Agent 0.08/h", "tag": "AGENT"},
        {"title": "Meta Muse Spark 首个闭源", "tag": "STRATEGY"}
      ],
      "footer_slogan": "briefing-v3 · 每日早读"
    }
"""

import argparse
import json
import os
import sys
import textwrap
from pathlib import Path

try:
    from PIL import Image, ImageDraw, ImageFont, ImageFilter
except ImportError:
    print("ERROR: Pillow not installed", file=sys.stderr)
    sys.exit(2)


# ----- Colour + layout constants -----------------------------------------
# Newspaper-off-white background, deep navy text, warm accent red.

BG_MAIN = (246, 243, 236)    # F6F3EC warm cream
BG_PANEL = (238, 233, 221)   # EEE9DD slightly darker panel
BG_PANEL_DARK = (28, 30, 46) # 1C1E2E deep navy reversed panel
INK_MAIN = (22, 22, 22)      # near-black body text
INK_SOFT = (85, 85, 85)      # grey secondary text
INK_MUTED = (130, 130, 130)  # muted caption
ACCENT_RED = (193, 55, 42)   # editorial brand red
ACCENT_BLUE = (31, 58, 118)  # masthead blue
RULE = (55, 55, 55)          # horizontal rule colour


# ----- Font loading -------------------------------------------------------

def load_font(path, size):
    """Load a TrueType font, returning the default font on failure
    so the script never crashes on a missing font."""
    try:
        return ImageFont.truetype(path, size=size)
    except Exception:
        return ImageFont.load_default()


# ----- Text wrapping ------------------------------------------------------

def wrap_by_width(draw, text, font, max_width):
    """Greedy character-level wrap for CJK. Returns a list of lines."""
    if not text:
        return []
    out = []
    line = ""
    for ch in text:
        if ch == "\n":
            out.append(line)
            line = ""
            continue
        test = line + ch
        bbox = draw.textbbox((0, 0), test, font=font)
        if bbox[2] - bbox[0] > max_width and line:
            out.append(line)
            line = ch
        else:
            line = test
    if line:
        out.append(line)
    return out


def draw_wrapped(draw, xy, text, font, fill, max_width, line_spacing=1.25, max_lines=None):
    """Draw wrapped text. Returns the y-coordinate just below the
    last drawn line."""
    lines = wrap_by_width(draw, text, font, max_width)
    if max_lines is not None and len(lines) > max_lines:
        lines = lines[:max_lines]
        # Append ellipsis to the last kept line if it was truncated.
        if lines:
            lines[-1] = lines[-1].rstrip() + "…"
    x, y = xy
    bbox = draw.textbbox((0, 0), "好", font=font)
    lh = int((bbox[3] - bbox[1]) * line_spacing)
    for line in lines:
        draw.text((x, y), line, font=font, fill=fill)
        y += lh
    return y


# ----- Common chrome (masthead + footer bar) -----------------------------

def draw_masthead(draw, w, left_text, right_text, font, fg=INK_MAIN):
    """Thin top strip with left/right corner labels + horizontal rule."""
    pad_x = 56
    y = 46
    if left_text:
        draw.text((pad_x, y), left_text.upper(), font=font, fill=fg)
    if right_text:
        bbox = draw.textbbox((0, 0), right_text.upper(), font=font)
        draw.text((w - pad_x - (bbox[2] - bbox[0]), y),
                  right_text.upper(), font=font, fill=fg)
    # Rule below.
    bbox = draw.textbbox((0, 0), "A", font=font)
    rule_y = y + (bbox[3] - bbox[1]) + 18
    draw.line([(pad_x, rule_y), (w - pad_x, rule_y)], fill=RULE, width=2)


def draw_footer_bar(draw, w, h, left_text, right_text, font, fg=INK_MAIN):
    """Matching bottom strip with brand + technical label."""
    pad_x = 56
    y = h - 80
    draw.line([(pad_x, y), (w - pad_x, y)], fill=RULE, width=2)
    ty = y + 22
    if left_text:
        draw.text((pad_x, ty), left_text.upper(), font=font, fill=fg)
    if right_text:
        bbox = draw.textbbox((0, 0), right_text.upper(), font=font)
        draw.text((w - pad_x - (bbox[2] - bbox[0]), ty),
                  right_text.upper(), font=font, fill=fg)


# ----- Item card renderer -------------------------------------------------

def render_item_card(data, output_path, width, height,
                     font_bold_path, font_regular_path):
    """One 1600x1600 info card per news item."""
    img = Image.new("RGB", (width, height), BG_MAIN)
    draw = ImageDraw.Draw(img)

    # Font scale: nominal sizes at 1600 canvas; scale if caller
    # overrode width/height.
    scale = width / 1600

    f_mono = load_font(font_bold_path, int(24 * scale))
    f_title = load_font(font_bold_path, int(84 * scale))
    f_subtitle = load_font(font_bold_path, int(42 * scale))
    f_intro = load_font(font_regular_path, int(34 * scale))
    f_hero_num = load_font(font_bold_path, int(220 * scale))
    f_hero_lbl = load_font(font_regular_path, int(30 * scale))
    f_stat_num = load_font(font_bold_path, int(80 * scale))
    f_stat_lbl = load_font(font_regular_path, int(26 * scale))
    f_section_h = load_font(font_bold_path, int(40 * scale))
    f_pt_title = load_font(font_bold_path, int(34 * scale))
    f_pt_desc = load_font(font_regular_path, int(28 * scale))
    f_footer = load_font(font_regular_path, int(26 * scale))
    f_category = load_font(font_bold_path, int(28 * scale))

    # Masthead (brand tag + category tag).
    draw_masthead(
        draw, width,
        left_text=data.get("brand_tag", "BRIEFING · NEWS"),
        right_text=data.get("category_tag", ""),
        font=f_mono,
        fg=INK_MAIN,
    )

    pad_x = int(56 * scale)
    content_top = int(150 * scale)
    content_w = width - pad_x * 2

    # --- Main title (huge, deep navy) ---
    main_title = data.get("main_title") or "(missing title)"
    title_col_w = int(content_w * 0.58)
    hero_col_x = pad_x + title_col_w + int(40 * scale)

    y = content_top
    # Tag line above title (short thin rule + small accent mono)
    tag = data.get("category_tag", "").upper()
    if tag:
        draw.text((pad_x, y), tag, font=f_mono, fill=ACCENT_RED)
        draw.line(
            [(pad_x + int(120 * scale), y + int(14 * scale)),
             (pad_x + title_col_w, y + int(14 * scale))],
            fill=ACCENT_RED, width=2,
        )
        y += int(40 * scale)

    y = draw_wrapped(draw, (pad_x, y), main_title, f_title,
                     INK_MAIN, title_col_w, line_spacing=1.15, max_lines=3)
    y += int(16 * scale)

    # Subtitle (accent red, smaller)
    subtitle = data.get("subtitle", "")
    if subtitle:
        y = draw_wrapped(draw, (pad_x, y), subtitle, f_subtitle,
                         ACCENT_RED, title_col_w, line_spacing=1.2, max_lines=2)
        y += int(24 * scale)

    # Intro paragraph.
    intro = data.get("intro", "")
    if intro:
        y = draw_wrapped(draw, (pad_x, y), intro, f_intro,
                         INK_MAIN, title_col_w, line_spacing=1.5, max_lines=6)

    # --- Hero number (top-right column) ---
    hero_num = str(data.get("hero_number") or "")
    hero_lbl = data.get("hero_label", "")
    if hero_num:
        hero_col_w = width - hero_col_x - pad_x
        # Dark panel behind hero number
        panel_pad = int(26 * scale)
        panel_left = hero_col_x - panel_pad
        panel_top = content_top
        panel_bottom = content_top + int(380 * scale)
        panel_right = width - pad_x
        draw.rectangle(
            [panel_left, panel_top, panel_right, panel_bottom],
            fill=BG_PANEL_DARK,
        )
        # Number centred horizontally.
        num_font = f_hero_num
        # Autoshrink if it overflows the panel width.
        while num_font.size > int(90 * scale):
            bbox = draw.textbbox((0, 0), hero_num, font=num_font)
            if bbox[2] - bbox[0] <= panel_right - panel_left - int(48 * scale):
                break
            num_font = load_font(font_bold_path, num_font.size - 8)
        bbox = draw.textbbox((0, 0), hero_num, font=num_font)
        nx = panel_left + (panel_right - panel_left - (bbox[2] - bbox[0])) // 2
        ny = panel_top + int(50 * scale)
        draw.text((nx, ny), hero_num, font=num_font, fill=BG_MAIN)
        # Label below.
        if hero_lbl:
            bbox_lbl = draw.textbbox((0, 0), hero_lbl, font=f_hero_lbl)
            lx = panel_left + (panel_right - panel_left - (bbox_lbl[2] - bbox_lbl[0])) // 2
            ly = ny + (bbox[3] - bbox[1]) + int(30 * scale)
            draw.text((lx, ly), hero_lbl, font=f_hero_lbl, fill=(200, 200, 210))

    # --- Stat numbers row (below hero panel on the right) ---
    stats = data.get("stat_numbers") or []
    if stats and hero_num:
        stat_top = content_top + int(410 * scale)
        stat_area_x = hero_col_x - int(26 * scale)
        stat_area_w = width - stat_area_x - pad_x
        cell_w = stat_area_w // max(1, min(2, len(stats)))
        for i, s in enumerate(stats[:2]):
            cx = stat_area_x + i * cell_w + int(18 * scale)
            draw.text((cx, stat_top),
                      str(s.get("value", "")), font=f_stat_num, fill=INK_MAIN)
            label = s.get("label", "")
            if label:
                draw.text((cx, stat_top + int(90 * scale)),
                          label, font=f_stat_lbl, fill=INK_SOFT)

    # --- Key points panel (bottom half) ---
    points = data.get("key_points") or []
    if points:
        panel_top = int(height * 0.60)
        panel_bottom = int(height * 0.86)
        draw.rectangle([pad_x, panel_top, width - pad_x, panel_bottom],
                       fill=BG_PANEL)
        # Panel heading
        draw.text((pad_x + int(28 * scale), panel_top + int(28 * scale)),
                  "三大要点 · KEY POINTS", font=f_section_h, fill=INK_MAIN)
        draw.line(
            [(pad_x + int(28 * scale), panel_top + int(90 * scale)),
             (width - pad_x - int(28 * scale), panel_top + int(90 * scale))],
            fill=RULE, width=2,
        )
        n = min(3, len(points))
        if n > 0:
            col_w = (width - pad_x * 2 - int(56 * scale)) // n
            for i in range(n):
                pt = points[i]
                cx = pad_x + int(28 * scale) + i * col_w + int(20 * scale)
                cy = panel_top + int(120 * scale)
                # Divider between columns
                if i > 0:
                    draw.line(
                        [(cx - int(20 * scale), panel_top + int(110 * scale)),
                         (cx - int(20 * scale), panel_bottom - int(40 * scale))],
                        fill=(220, 215, 200), width=2,
                    )
                draw.text((cx, cy), pt.get("title", ""),
                          font=f_pt_title, fill=ACCENT_BLUE)
                cy += int(60 * scale)
                draw_wrapped(draw, (cx, cy), pt.get("desc", ""),
                             f_pt_desc, INK_MAIN,
                             col_w - int(40 * scale),
                             line_spacing=1.45, max_lines=5)

    # --- Footer summary line (just above footer bar) ---
    footer_sum = data.get("footer_summary", "")
    if footer_sum:
        y = int(height * 0.88)
        draw_wrapped(draw, (pad_x, y), footer_sum, f_footer,
                     INK_SOFT, content_w, line_spacing=1.4, max_lines=2)

    # --- Footer bar ---
    draw_footer_bar(
        draw, width, height,
        left_text="briefing-v3 · editorial",
        right_text="INFO CARD · 1600 × 1600",
        font=f_mono,
    )

    img.save(output_path, "PNG", optimize=True)


# ----- Header card (page hero) -------------------------------------------

def render_header_card(data, output_path, width, height,
                       font_bold_path, font_regular_path):
    """One big banner for the whole issue — the 大字报 top of page.

    v1.0.0 急救版：从原来 7 个稀疏元素扩展到 11 个区域，把 1600x1600
    画布从 ~40% 留白填到 ~90% 密度。新区域：edition 期号、加大的
    main_headline、lead_paragraph 导语段、6 条 stories 双行布局、
    key_numbers 横排数字。LLM prompt 同步加长字数限制，让每个区域
    都有内容可填。
    """
    img = Image.new("RGB", (width, height), BG_MAIN)
    draw = ImageDraw.Draw(img)

    scale = width / 1600

    f_mono = load_font(font_bold_path, int(26 * scale))
    f_edition = load_font(font_bold_path, int(34 * scale))
    f_date = load_font(font_bold_path, int(40 * scale))
    f_headline = load_font(font_bold_path, int(108 * scale))
    f_sub = load_font(font_bold_path, int(46 * scale))
    f_lead = load_font(font_regular_path, int(34 * scale))
    f_section_label = load_font(font_bold_path, int(24 * scale))
    f_story_tag = load_font(font_bold_path, int(24 * scale))
    f_story_title = load_font(font_bold_path, int(34 * scale))
    f_keynum_value = load_font(font_bold_path, int(96 * scale))
    f_keynum_label = load_font(font_regular_path, int(24 * scale))
    f_slogan = load_font(font_regular_path, int(28 * scale))

    # Masthead
    draw_masthead(
        draw, width,
        left_text="BRIEFING-V3 · DAILY",
        right_text="AI INSIGHT DAILY",
        font=f_mono,
    )

    pad_x = int(56 * scale)
    content_w = width - pad_x * 2

    # ---- Edition + Date line: red bar | issue_date | edition ----------
    y = int(120 * scale)
    draw.rectangle(
        [pad_x, y + int(6 * scale), pad_x + int(12 * scale), y + int(44 * scale)],
        fill=ACCENT_RED,
    )
    issue_date = data.get("issue_date", "")
    draw.text((pad_x + int(28 * scale), y), issue_date.upper(),
              font=f_date, fill=ACCENT_RED)
    edition = data.get("edition", "").strip()
    if edition:
        edition_bbox = draw.textbbox((0, 0), edition, font=f_edition)
        edition_w = edition_bbox[2] - edition_bbox[0]
        draw.text(
            (width - pad_x - edition_w, y + int(2 * scale)),
            edition,
            font=f_edition,
            fill=INK_SOFT,
        )

    # ---- Main headline (huge, up to 4 lines) --------------------------
    y += int(80 * scale)
    headline = data.get("main_headline") or "AI 资讯日报"
    y = draw_wrapped(draw, (pad_x, y), headline, f_headline,
                     INK_MAIN, content_w, line_spacing=1.12, max_lines=4)
    y += int(20 * scale)

    # ---- Sub-headline (blue accent) -----------------------------------
    sub = data.get("sub_headline", "")
    if sub:
        y = draw_wrapped(draw, (pad_x, y), sub, f_sub,
                         ACCENT_BLUE, content_w, line_spacing=1.28, max_lines=2)
        y += int(28 * scale)

    # ---- Horizontal rule before lead paragraph ------------------------
    draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=3)
    y += int(28 * scale)

    # ---- Lead paragraph (导语段) --------------------------------------
    # New v1.0.0 region: 100-160 字 narrative summarising today's top
    # stories in a single connected paragraph, like a real newspaper lead.
    lead = data.get("lead_paragraph", "").strip()
    if lead:
        y = draw_wrapped(
            draw, (pad_x, y), lead, f_lead,
            INK_MAIN, content_w, line_spacing=1.45, max_lines=6,
        )
        y += int(36 * scale)
        # Section divider rule
        draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=2)
        y += int(28 * scale)

    # ---- Top stories block (6 entries, 3 columns × 2 rows) ------------
    stories = (data.get("top_stories") or [])[:6]
    if stories:
        # "TOP STORIES" section label on the left
        draw.text((pad_x, y), "TOP STORIES",
                  font=f_section_label, fill=ACCENT_RED)
        y += int(40 * scale)

        col_gap = int(40 * scale)
        row_gap = int(30 * scale)
        col_w = (content_w - col_gap * 2) // 3
        # Each story cell is roughly 140px high in 1600 frame.
        row_h = int(170 * scale)

        for i, st in enumerate(stories):
            row = i // 3
            col = i % 3
            cx = pad_x + col * (col_w + col_gap)
            cy = y + row * (row_h + row_gap)
            tag = st.get("tag", "").upper()
            if tag:
                draw.text((cx, cy), tag, font=f_story_tag, fill=ACCENT_RED)
                cy += int(34 * scale)
            draw_wrapped(
                draw, (cx, cy), st.get("title", ""),
                f_story_title, INK_MAIN, col_w,
                line_spacing=1.3, max_lines=3,
            )

        n_rows = (len(stories) + 2) // 3
        y += n_rows * (row_h + row_gap) + int(20 * scale)

        # Section divider rule
        draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=2)
        y += int(40 * scale)

    # ---- Key numbers strip (3 cells with huge value + label) ---------
    # New v1.0.0 region: editorial-style "by the numbers" panel.
    key_numbers = (data.get("key_numbers") or [])[:3]
    if key_numbers:
        draw.text((pad_x, y), "BY THE NUMBERS",
                  font=f_section_label, fill=ACCENT_RED)
        y += int(36 * scale)

        cell_w = content_w // len(key_numbers)
        for i, kn in enumerate(key_numbers):
            cx = pad_x + i * cell_w
            value = (kn.get("value") or "").strip()
            label = (kn.get("label") or "").strip()
            if value:
                draw.text((cx, y), value, font=f_keynum_value, fill=ACCENT_BLUE)
            if label:
                draw.text(
                    (cx, y + int(108 * scale)),
                    label,
                    font=f_keynum_label,
                    fill=INK_SOFT,
                )

        y += int(160 * scale)

    # ---- Footer slogan (centered) -------------------------------------
    slogan = data.get("footer_slogan", "briefing-v3 · 每日早读")
    sy = int(height * 0.93)
    bbox = draw.textbbox((0, 0), slogan, font=f_slogan)
    sx = (width - (bbox[2] - bbox[0])) // 2
    draw.text((sx, sy), slogan, font=f_slogan, fill=INK_SOFT)

    # ---- Footer bar ---------------------------------------------------
    draw_footer_bar(
        draw, width, height,
        left_text="briefing-v3 · hero",
        right_text="HEADLINE · 1600 × 1600",
        font=f_mono,
    )

    img.save(output_path, "PNG", optimize=True)


# ----- CLI ---------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["item", "header"], required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--width", type=int, default=1600)
    ap.add_argument("--height", type=int, default=1600)
    ap.add_argument("--font-bold",
                    default="/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc")
    ap.add_argument("--font-regular",
                    default="/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc")
    ap.add_argument("--json-file",
                    help="read JSON from file instead of stdin (optional)")
    args = ap.parse_args()

    # Load JSON payload
    if args.json_file:
        with open(args.json_file, "r", encoding="utf-8") as f:
            data = json.load(f)
    else:
        data = json.load(sys.stdin)

    os.makedirs(os.path.dirname(args.output) or ".", exist_ok=True)

    if args.mode == "item":
        render_item_card(
            data, args.output, args.width, args.height,
            args.font_bold, args.font_regular,
        )
    else:
        render_header_card(
            data, args.output, args.width, args.height,
            args.font_bold, args.font_regular,
        )

    print(f"OK: {args.output}", file=sys.stdout)


if __name__ == "__main__":
    main()
