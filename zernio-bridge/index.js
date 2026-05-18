import Zernio from '@zernio/node';
import { createCanvas } from '@napi-rs/canvas';
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

// Upload media file, returns publicUrl for use in /post-with-media
app.post('/upload', upload.single('file'), async (req, res) => {
  if (!req.file) return res.status(400).json({ error: 'file is required' });
  try {
    const { data: up } = await zernio.media.getMediaPresignedUrl({
      body: { filename: req.file.originalname, contentType: req.file.mimetype },
    });
    const fileBuffer = fs.readFileSync(req.file.path);
    await fetch(up.uploadUrl, {
      method: 'PUT',
      headers: { 'Content-Type': req.file.mimetype },
      body: fileBuffer,
    });
    try { fs.unlinkSync(req.file.path); } catch {}
    res.json({ ok: true, publicUrl: up.publicUrl });
  } catch (err) {
    try { fs.unlinkSync(req.file.path); } catch {}
    res.status(500).json({ error: err.message });
  }
});

// Post with media URLs (image or video)
app.post('/post-with-media', async (req, res) => {
  const { content, platforms, mediaUrls, scheduledFor } = req.body;
  if (!content || !platforms?.length || !mediaUrls?.length) {
    return res.status(400).json({ error: 'content, platforms, and mediaUrls are required' });
  }
  try {
    const { data } = await zernio.posts.createPost({
      body: {
        content,
        platforms: platforms.map(p => ({ platform: p.platform, accountId: p.accountId })),
        mediaItems: mediaUrls.map(url => ({ type: 'image', url })),
        ...(scheduledFor ? { scheduledFor } : { publishNow: true }),
      },
    });
    res.json({ ok: true, post: data });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Generate a branded 1080x1080 image card and post to platforms in one call.
// Body: { cardText: "text for image (no hashtags)", content: "full caption with hashtags", platforms: [...] }
app.post('/generate-card', async (req, res) => {
  const { cardText, content, platforms } = req.body;
  if (!cardText || !content || !platforms?.length) {
    return res.status(400).json({ error: 'cardText, content, and platforms are required' });
  }
  try {
    const W = 1080, H = 1080;
    const BG = '#1B4332';
    const ACCENT = '#52B788';

    const canvas = createCanvas(W, H);
    const ctx = canvas.getContext('2d');

    ctx.fillStyle = BG;
    ctx.fillRect(0, 0, W, H);

    ctx.fillStyle = ACCENT;
    ctx.fillRect(0, 0, W, 115);

    ctx.fillStyle = BG;
    ctx.font = 'bold 36px sans-serif';
    ctx.fillText('Minnesota EquiVoice Partnership', 40, 68);

    ctx.fillStyle = '#FFFFFF';
    ctx.font = '30px sans-serif';
    const lines = wrapText(ctx, cardText, W - 120);
    let y = 155;
    for (const line of lines.slice(0, 18)) {
      if (y > H - 120) break;
      ctx.fillText(line, 60, y);
      y += 46;
    }

    ctx.fillStyle = ACCENT;
    ctx.fillRect(0, H - 90, W, 90);
    ctx.fillStyle = BG;
    ctx.font = 'bold 24px sans-serif';
    ctx.fillText('mnequivoicepartnership.org', 40, H - 50);

    const buffer = canvas.toBuffer('image/png');

    const { data: up } = await zernio.media.getMediaPresignedUrl({
      body: { filename: 'equivoice_card.png', contentType: 'image/png' },
    });
    await fetch(up.uploadUrl, {
      method: 'PUT',
      headers: { 'Content-Type': 'image/png' },
      body: buffer,
    });

    const { data: post } = await zernio.posts.createPost({
      body: {
        content,
        platforms: platforms.map(p => ({ platform: p.platform, accountId: p.accountId })),
        mediaItems: [{ type: 'image', url: up.publicUrl }],
        publishNow: true,
      },
    });
    res.json({ ok: true, post });
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

const PORT = process.env.PORT || 3001;
app.listen(PORT, '0.0.0.0', () => console.log(`Zernio bridge listening on 0.0.0.0:${PORT}`));
