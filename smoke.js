// Minimal Playwright smoke test: launch the chromium
// you downloaded (D:\chrome-win\chrome-win\chrome.exe),
// navigate to about:blank, take a screenshot. Proves
// the test driver works before we wire up the real
// UI test.
//
// Run:  node D:\mavis-tmp\playwright-smoke.js
const { chromium } = require('playwright');
const path = require('path');

(async () => {
  const browser = await chromium.launch({
    executablePath: 'D:\\\\chrome-win\\\\chrome-win\\\\chrome.exe',
    headless: true,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto('about:blank');
  const title = await page.title();
  console.log('title:', title);
  await page.screenshot({ path: 'D:\\\\mavis-tmp\\\\smoke.png' });
  await browser.close();
  console.log('OK');
})().catch((e) => { console.error('FAIL:', e); process.exit(1); });
