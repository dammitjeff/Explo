#!/usr/bin/env node
// Playlist cover art generator. Reads JSON from stdin, writes PNG to stdout.
// Input: { playlistType, artists: string[], coverUrls: string[] }

import { createCanvas, loadImage, GlobalFonts } from '@napi-rs/canvas';
import { existsSync, mkdirSync, writeFileSync } from 'fs';
import { dirname, join } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const FONTS_DIR = join(__dirname, 'fonts');

// ── Fonts: auto-download to scripts/fonts/ on first use ──────────────────────

const FONT_URLS = {
  'Bebas Neue':  'https://raw.githubusercontent.com/google/fonts/main/ofl/bebasneue/BebasNeue-Regular.ttf',
  'Nunito Sans': 'https://raw.githubusercontent.com/google/fonts/main/ofl/nunitosans/NunitoSans%5BYTLC%2Copsz%2Cwdth%2Cwght%5D.ttf',
};

async function ensureFont(family) {
  const file = join(FONTS_DIR, family.replace(/\s+/g, '') + '.ttf');
  if (existsSync(file)) return GlobalFonts.registerFromPath(file, family);
  try {
    const res = await fetch(FONT_URLS[family]);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const buf = Buffer.from(await res.arrayBuffer());
    mkdirSync(FONTS_DIR, { recursive: true });
    writeFileSync(file, buf);
    return GlobalFonts.registerFromPath(file, family);
  } catch (e) {
    process.stderr.write(`[cover] font "${family}" unavailable: ${e.message}\n`);
    return false;
  }
}

// ── Input ─────────────────────────────────────────────────────────────────────

const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const { playlistType, artists = [], coverUrls = [] } = JSON.parse(Buffer.concat(chunks));

const [hasBebas, hasNunito] = await Promise.all([
  ensureFont('Bebas Neue'),
  ensureFont('Nunito Sans'),
]);

// ── Presets (matches PlaylistCard.jsx) ────────────────────────────────────────

const PRESETS = {
  'weekly-exploration': ['#2979ff', '#7c3aed', '#0ea5e9'],
  'weekly-jams':        ['#fb923c', '#ef4444', '#f472b6'],
  'daily-jams':         ['#10b981', '#06b6d4', '#22c55e'],
  'on-repeat':          ['#e11d48', '#9f1239', '#fb7185'],
};

const TITLE_LINES = {
  'weekly-exploration': ['WEEKLY', 'EXPLORATION'],
  'weekly-jams':        ['WEEKLY', 'JAMS'],
  'daily-jams':         ['DAILY', 'JAMS'],
  'on-repeat':          ['ON', 'REPEAT'],
};

const [shadow, midtone, highlight] = PRESETS[playlistType] ?? ['#646478', '#50506e', '#828296'];
const titleLines = TITLE_LINES[playlistType] ?? playlistType.toUpperCase().replace(/-/g, ' ').split(' ');

// ── Canvas setup ──────────────────────────────────────────────────────────────

const W = 800, H = 800;
const PAD = 36;
const BAR_H = 108;
const BAR_Y = H - BAR_H;

const canvas = createCanvas(W, H);
const ctx = canvas.getContext('2d');

const aa = (hex, a) => hex + Math.round(a * 255).toString(16).padStart(2, '0');

// ── 1. Base fill ──────────────────────────────────────────────────────────────

ctx.fillStyle = shadow;
ctx.fillRect(0, 0, W, H);

// ── 2. Album art — grayscale, cover-cropped ───────────────────────────────────

let artLoaded = false;
if (coverUrls.length > 0) {
  const url = coverUrls[Math.floor(Math.random() * coverUrls.length)];
  try {
    const art = await loadImage(url);
    const scale = Math.max(W / art.width, H / art.height);
    const sw = art.width * scale, sh = art.height * scale;
    ctx.save();
    ctx.filter = 'grayscale(1) contrast(1.1) brightness(0.5)';
    ctx.drawImage(art, (W - sw) / 2, (H - sh) / 2, sw, sh);
    ctx.restore();
    artLoaded = true;
  } catch { /* fallback: gradient only */ }
}

// ── 3. Strong colour tint (heavier when art is present) ───────────────────────

const grad = ctx.createLinearGradient(0, 0, W, H);
grad.addColorStop(0,    aa(shadow,    artLoaded ? 0.74 : 0.36));
grad.addColorStop(0.48, aa(midtone,   artLoaded ? 0.64 : 0.28));
grad.addColorStop(1,    aa(highlight, artLoaded ? 0.54 : 0.18));
ctx.fillStyle = grad;
ctx.fillRect(0, 0, W, H);

// ── 4. Slight overall darkening for legibility ────────────────────────────────

ctx.fillStyle = 'rgba(0,0,0,0.18)';
ctx.fillRect(0, 0, W, H);

// ── 5. Bottom artist bar ──────────────────────────────────────────────────────

ctx.fillStyle = 'rgba(0,0,0,0.58)';
ctx.fillRect(0, BAR_Y, W, BAR_H);

// ── 6. Text ───────────────────────────────────────────────────────────────────

const titleFamily = hasBebas   ? '"Bebas Neue"'  : 'bold sans-serif';
const artistFamily = hasNunito ? '"Nunito Sans"' : 'sans-serif';

// — explo stamp (top-right) —
ctx.save();
ctx.font = `56px ${titleFamily}`;
ctx.fillStyle = 'rgba(255,255,255,0.52)';
ctx.textAlign = 'right';
ctx.fillText('explo', W - PAD, PAD + 48);
ctx.restore();

// — Main title: large, left-aligned, vertically centred above bar —
const longest = titleLines.reduce((a, b) => b.length > a.length ? b : a, '');
const maxTitleW = W - PAD * 2;

let fontSize = 220;
ctx.font = `${fontSize}px ${titleFamily}`;
while (fontSize > 56 && ctx.measureText(longest).width > maxTitleW) {
  fontSize -= 4;
  ctx.font = `${fontSize}px ${titleFamily}`;
}

// Centre the visual block of caps vertically in the space above the bar
const lineH  = fontSize;
const capH   = fontSize * 0.72;  // Bebas Neue cap height ≈ 72% of em
const blockH = lineH * (titleLines.length - 1) + capH;
let lineY    = (BAR_Y / 2) - (blockH / 2) + capH;

ctx.save();
ctx.fillStyle = 'white';
ctx.shadowColor = 'rgba(0,0,0,0.4)';
ctx.shadowBlur = 14;
ctx.textAlign = 'left';
for (const line of titleLines) {
  ctx.fillText(line, PAD, lineY);
  lineY += lineH;
}
ctx.restore();

// — Artist names (centred in bar) —
if (artists.length > 0) {
  ctx.save();
  ctx.font = `600 27px ${artistFamily}`;
  ctx.fillStyle = 'rgba(255,255,255,0.88)';
  ctx.textAlign = 'center';

  const sep = '  ·  ';
  let text = artists.slice(0, 3).join(sep);
  const maxW = W - PAD * 4;
  if (ctx.measureText(text).width > maxW) {
    while (text.length > 1 && ctx.measureText(text + '…').width > maxW) {
      text = text.slice(0, -1);
    }
    text = text.trimEnd() + '…';
  }

  ctx.fillText(text, W / 2, BAR_Y + BAR_H * 0.62);
  ctx.restore();
}

// ── Output ────────────────────────────────────────────────────────────────────

const png = await canvas.encode('png');
process.stdout.write(png);
