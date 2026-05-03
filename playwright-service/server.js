const express = require('express');
const { chromium } = require('playwright');

const app = express();
app.use(express.json({ limit: '10mb' }));

let browser;

async function getBrowser() {
  if (!browser || !browser.isConnected()) {
    browser = await chromium.launch({
      headless: true,
      args: ['--no-sandbox', '--disable-setuid-sandbox']
    });
  }
  return browser;
}

app.get('/health', (req, res) => {
  res.json({ status: 'ok' });
});

// Render a page: returns HTML, text, links, and optional screenshot
app.post('/render', async (req, res) => {
  const { url, screenshot = false, timeout = 30000 } = req.body;

  if (!url) {
    return res.status(400).json({ error: 'url is required' });
  }

  let context;
  let page;
  try {
    const b = await getBrowser();
    context = await b.newContext({
      userAgent: 'Mozilla/5.0 (compatible; SimpleScraper/1.0)',
      viewport: { width: 1280, height: 800 }
    });
    page = await context.newPage();

    await page.goto(url, { waitUntil: 'networkidle', timeout });

    const html = await page.content();
    const text = await page.evaluate(() => document.body ? document.body.innerText : '');
    const title = await page.title();

    const links = await page.evaluate(() => {
      const anchors = Array.from(document.querySelectorAll('a[href]'));
      return anchors.map(a => a.href).filter(h => h && (h.startsWith('http://') || h.startsWith('https://')));
    });

    const result = {
      url,
      title,
      html,
      text,
      links: [...new Set(links)]
    };

    if (screenshot) {
      const buf = await page.screenshot({ fullPage: true, type: 'png' });
      result.screenshot = buf.toString('base64');
    }

    res.json(result);
  } catch (err) {
    console.error('Render error:', err.message);
    res.status(500).json({ error: err.message });
  } finally {
    if (page) await page.close().catch(() => {});
    if (context) await context.close().catch(() => {});
  }
});

const PORT = process.env.PORT || 3000;
app.listen(PORT, () => {
  console.log(`Playwright service listening on port ${PORT}`);
});

process.on('SIGTERM', async () => {
  if (browser) await browser.close();
  process.exit(0);
});
