import Zernio from '@zernio/node';
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
app.listen(PORT, '127.0.0.1', () => console.log(`Zernio bridge listening on 127.0.0.1:${PORT}`));
