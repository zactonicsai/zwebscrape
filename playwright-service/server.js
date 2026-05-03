const express = require('express');
const { chromium } = require('playwright');

const app = express();
app.use(express.json({ limit: '20mb' }));

let browser;

async function getBrowser() {
  if (!browser || !browser.isConnected()) {
    browser = await chromium.launch({
      headless: true,
      args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage']
    });
  }
  return browser;
}

app.get('/health', (req, res) => {
  res.json({ status: 'ok' });
});

// Legacy endpoint used by older clients & by api_test.go.
// Returns html + text + links + (optional) screenshot, no clicking.
app.post('/render', async (req, res) => {
  const { url, screenshot = false, timeout = 30000 } = req.body || {};
  if (!url) return res.status(400).json({ error: 'url is required' });

  let context, page;
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

    const result = { url, title, html, text, links: [...new Set(links)], clicks: [] };
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

// Main scrape endpoint:
// - loads the page
// - captures initial html / text / screenshot / links
// - finds every clickable element (button, role=button, [onclick], input[type=button|submit])
// - clicks each one in turn, captures post-click html + text
// - resets navigation between clicks if a click navigated away
// - bounded by max_clicks to keep work finite
app.post('/scrape', async (req, res) => {
  const {
    url,
    screenshot = true,
    click_buttons = true,
    max_clicks = 20,
    timeout = 45000
  } = req.body || {};

  if (!url) return res.status(400).json({ error: 'url is required' });

  let context, page;
  const t0 = Date.now();
  try {
    const b = await getBrowser();
    context = await b.newContext({
      userAgent: 'Mozilla/5.0 (compatible; SimpleScraper/1.0)',
      viewport: { width: 1280, height: 900 },
      // Block obvious noise so clicks don't try to open ad SDKs
      bypassCSP: true
    });
    // Auto-dismiss any dialog spawned by clicks
    context.on('page', p => p.on('dialog', d => d.dismiss().catch(() => {})));

    page = await context.newPage();
    page.on('dialog', d => d.dismiss().catch(() => {}));

    await page.goto(url, { waitUntil: 'networkidle', timeout });

    const initial = await captureState(page);
    const links = await collectLinks(page);

    let clicks = [];
    if (click_buttons) {
      clicks = await clickAllButtons(page, url, max_clicks, timeout);
    }

    const result = {
      url: initial.url,
      title: initial.title,
      html: initial.html,
      text: initial.text,
      links,
      clicks
    };
    if (screenshot) {
      try {
        const buf = await page.screenshot({ fullPage: true, type: 'png' });
        result.screenshot = buf.toString('base64');
      } catch (e) {
        // Screenshot on a navigated/destroyed page can fail — non-fatal
      }
    }

    console.log(`scrape ${url} -> ${clicks.length} clicks in ${Date.now()-t0}ms`);
    res.json(result);
  } catch (err) {
    console.error('Scrape error:', err.message);
    res.status(500).json({ error: err.message });
  } finally {
    if (page) await page.close().catch(() => {});
    if (context) await context.close().catch(() => {});
  }
});

// Capture {url, title, html, text} for the current page state.
async function captureState(page) {
  const url = page.url();
  let title = '';
  try { title = await page.title(); } catch {}
  let html = '';
  try { html = await page.content(); } catch {}
  let text = '';
  try { text = await page.evaluate(() => document.body ? document.body.innerText : ''); } catch {}
  return { url, title, html, text };
}

async function collectLinks(page) {
  try {
    const links = await page.evaluate(() => {
      const anchors = Array.from(document.querySelectorAll('a[href]'));
      return anchors.map(a => a.href).filter(h => h && (h.startsWith('http://') || h.startsWith('https://')));
    });
    return [...new Set(links)];
  } catch {
    return [];
  }
}

// Find every "clickable" element, click it, capture the post-click state.
//
// Handles edge cases:
// - clicking causes navigation: return to original URL before next click
// - clicking opens a new tab: close the new tab
// - element no longer exists after a click: skip
// - element is hidden/disabled: skip
// - dedupe by stable signature so we don't click the same logical button 5x
async function clickAllButtons(page, originalURL, maxClicks, timeout) {
  const out = [];

  // Inspect the DOM and produce a list of candidate clickables with stable keys
  const candidates = await page.evaluate(() => {
    const sel = [
      'button',
      '[role="button"]',
      'input[type="button"]',
      'input[type="submit"]',
      '[onclick]',
      'summary',
    ].join(',');
    const els = Array.from(document.querySelectorAll(sel));
    const seen = new Set();
    const out = [];
    for (const el of els) {
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) continue;
      if (el.disabled || el.getAttribute('aria-disabled') === 'true') continue;

      const text = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim().slice(0, 80);
      const id   = el.id || '';
      const name = el.getAttribute('name') || '';
      const tag  = el.tagName.toLowerCase();

      const key = `${tag}|${id}|${name}|${text}`;
      if (seen.has(key)) continue;
      seen.add(key);

      out.push({
        text,
        id,
        name,
        tag,
        // a stable selector we can use later
        selector: id ? `#${cssEscape(id)}` :
                  name ? `${tag}[name="${cssEscape(name)}"]` :
                  tag === 'button' && text ? `button:has-text(${JSON.stringify(text)})` :
                  text ? `${tag}:has-text(${JSON.stringify(text)})` :
                  tag,
      });
    }
    function cssEscape(s) { return String(s).replace(/(["\\])/g, '\\$1'); }
    return out;
  });

  let step = 0;
  for (const cand of candidates) {
    if (step >= maxClicks) break;
    step++;

    try {
      // Always start each click from the original URL & DOM, otherwise the
      // selectors we recorded above can disappear.
      if (page.url() !== originalURL) {
        await page.goto(originalURL, { waitUntil: 'networkidle', timeout }).catch(() => {});
      }

      // Locate via Playwright. has-text selectors require the chromium engine.
      let locator;
      try {
        locator = page.locator(cand.selector).first();
      } catch (e) {
        continue;
      }

      // Make sure it's still there & visible
      const visible = await locator.isVisible({ timeout: 2000 }).catch(() => false);
      if (!visible) continue;

      // Track new tabs opened by clicks; close them.
      const popupPromise = page.waitForEvent('popup', { timeout: 1500 }).catch(() => null);

      // Actually click. Force=true bypasses some pointer-event blockers.
      await locator.click({ timeout: 5000, force: false }).catch(async () => {
        await locator.click({ timeout: 3000, force: true }).catch(() => {});
      });

      // Wait for any post-click work (animations, fetches) to settle
      await page.waitForLoadState('networkidle', { timeout: 5000 }).catch(() => {});

      const popup = await popupPromise;
      if (popup) await popup.close().catch(() => {});

      const after = await captureState(page);
      out.push({
        step,
        selector: cand.selector,
        label: cand.text || cand.id || cand.name || cand.tag,
        html: after.html,
        text: after.text
      });
    } catch (e) {
      // ignore individual click failures so one bad button doesn't kill the scan
      console.warn(`click step ${step} failed:`, e.message);
    }
  }

  // Final restoration so the caller can still take a screenshot of the seed page
  if (page.url() !== originalURL) {
    await page.goto(originalURL, { waitUntil: 'networkidle', timeout }).catch(() => {});
  }

  return out;
}

const PORT = process.env.PORT || 3000;
app.listen(PORT, () => {
  console.log(`Playwright service listening on port ${PORT}`);
});

process.on('SIGTERM', async () => {
  if (browser) await browser.close();
  process.exit(0);
});
