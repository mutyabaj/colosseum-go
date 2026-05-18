import Zernio from '@zernio/node';
import { createCanvas, GlobalFonts } from '@napi-rs/canvas';
import express from 'express';
import multer from 'multer';
import fs from 'fs';
import 'dotenv/config';

const app = express();
app.use(express.json());
const upload = multer({ dest: '/tmp/zernio-uploads/' });

const zernio = new Zernio({ apiKey: process.env.ZERNIO_API_KEY });

// Health check
app.get('/health', (req, res) => res.json({ ok: true }));

// List connected platform accounts
app.get('/accounts', async (req, res) => {
  try {
    const { data } = await zernio.accounts.listAccounts();
    res.json(data);
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Post text (optionally scheduled)
app.post('/post', async (req, res) => {
  const { content, platforms, scheduledFor } = req.body;
  if (!content || !platforms?.length) {
    return res.status(400).json({ error: 'content and platforms are required' });
  }
  try {
    const { data } = await zernio.posts.createPost({
      body: {
        content,
        platforms: platforms.map(p => ({ platform: p.platform, accountId: p.accountId })),
        ...(scheduledFor ? { scheduledFor } : { publishNow: true }),
      },
    });
    res.json({ ok: true, post: data });
  } catch (err) {
    res.status(500).json({ error: err.message, details: err.fields ?? null });
  }
});

// Upload media file, returns mediaId for use in /post-with-media
app.post('/upload', upload.single('file'), async (req, res) => {
  if (!req.file) return res.status(400).json({ error: 'file is required' });
  try {
    const { data: upload } = await zernio.media.getUploadUrl({
      body: { fileName: req.file.originalname, mimeType: req.file.mimetype },
    });
    const fileBuffer = fs.readFileSync(req.file.path);
    await fetch(upload.uploadUrl, {
      method: 'PUT',
      headers: { 'Content-Type': req.file.mimetype },
      body: fileBuffer,
    });
    fs.unlinkSync(req.file.path);
    res.json({ ok: true, mediaId: upload.mediaId });
  } catch (err) {
    fs.unlinkSync(req.file.path);
    res.status(500).json({ error: err.message });
  }
});

// Generate a branded image card and upload it to Zernio; returns mediaId
app.post('/generate-card', async (req, res) => {
  const { text } = req.body;
  if (!text) return res.status(400).json({ error: 'text is required' });
  try {
    const W = 1080, H = 1080;
    const BG = '#1B4332';
    const ACCENT = '#52B788';

    const canvas = createCanvas(W, H);
    const ctx = canvas.getContext('2d');

    // Background
    ctx.fillStyle = BG;
    ctx.fillRect(0, 0, W, H);

    // Top banner
    ctx.fillStyle = ACCENT;
    ctx.fillRect(0, 0, W, 115);

    // Org name on banner
    ctx.fillStyle = BG;
    ctx.font = 'bold 36px sans-serif';
    ctx.fillText('Minnesota EquiVoice Partnership', 40, 68);

    // Content text
    ctx.fillStyle = '#FFFFFF';
    ctx.font = '30px sans-serif';
    const lines = wrapText(ctx, text, W - 120);
    let y = 155;
    for (const line of lines.slice(0, 18)) {
      if (y > H - 120) break;
      ctx.fillText(line, 60, y);
      y += 46;
    }

    // Bottom banner
    ctx.fillStyle = ACCENT;
    ctx.fillRect(0, H - 90, W, 90);
    ctx.fillStyle = BG;
    ctx.font = 'bold 24px sans-serif';
    ctx.fillText('mnequivoicepartnership.org', 40, H - 50);

    const buffer = canvas.toBuffer('image/png');

    const { data: up } = await zernio.media.getUploadUrl({
      body: { fileName: 'equivoice_card.png', mimeType: 'image/png' },
    });
    await fetch(up.uploadUrl, {
      method: 'PUT',
      headers: { 'Content-Type': 'image/png' },
      body: buffer,
    });
    res.json({ ok: true, mediaId: up.mediaId });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

function wrapText(ctx, text, maxWidth) {
  const lines = [];
  for (const paragraph of text.split('\n')) {
    const words = paragraph.split(' ');
    let line = '';
    for (const word of words) {
      const test = line ? `${line} ${word}` : word;
      if (ctx.measureText(test).width > maxWidth && line) {
        lines.push(line);
        line = word;
      } else {
        line = test;
      }
    }
    if (line) lines.push(line);
  }
  return lines;
}

// Post with media (image or video)
app.post('/post-with-media', async (req, res) => {
  const { content, platforms, mediaIds, scheduledFor } = req.body;
  if (!content || !platforms?.length || !mediaIds?.length) {
    return res.status(400).json({ error: 'content, platforms, and mediaIds are required' });
  }
  try {
    const { data } = await zernio.posts.createPost({
      body: {
        content,
        platforms: platforms.map(p => ({ platform: p.platform, accountId: p.accountId })),
        mediaIds,
        ...(scheduledFor ? { scheduledFor } : { publishNow: true }),
      },
    });
    res.json({ ok: true, post: data });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

const PORT = process.env.PORT || 3001;
app.listen(PORT, '0.0.0.0', () => console.log(`Zernio bridge listening on 0.0.0.0:${PORT}`));
