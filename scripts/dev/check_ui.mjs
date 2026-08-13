#!/usr/bin/env node
// Headless smoke test for the Web Awesome migration.
// Install once with `npm install --prefix scripts/ui-check` and
// `npm exec --prefix scripts/ui-check playwright install chromium`, then run
// with `node scripts/dev/check_ui.mjs` against a running local dev server.
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require('../ui-check/node_modules/playwright');

const base = process.env.CYCLOPUS_UI_URL || 'http://127.0.0.1:8081';
const themes = ['obsidian', 'mono', 'ember', 'graphite', 'terminal', 'paper', 'console', 'solar'];
const icons = [
  'box',
  'chevron-right',
  'chevron-up',
  'circle-info',
  'code-branch',
  'gear',
  'key',
  'list',
  'palette',
  'plug',
  'right-from-bracket',
  'rotate',
  'table-cells-large',
  'xmark',
];
const browser = await chromium.launch(
  process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}
);
const page = await browser.newPage({ viewport: { width: 1000, height: 1400 } });
const errors = [];
page.on('pageerror', error => errors.push(error.message));
page.on('response', response => {
  if (response.status() >= 400 && !response.url().endsWith('/favicon.ico')) {
    errors.push(`${response.status()} ${response.url()}`);
  }
});

await page.goto(`${base}/__theme-preview`);
await page.waitForTimeout(2000);

const missingIcons = await page.evaluate(async names => {
  const elements = names.map(name => {
    const icon = document.createElement('wa-icon');
    icon.name = name;
    document.body.appendChild(icon);
    return icon;
  });
  await customElements.whenDefined('wa-icon');
  await Promise.all(elements.map(icon => icon.updateComplete));
  await new Promise(resolve => setTimeout(resolve, 1000));
  const missing = elements.filter(icon => !icon.shadowRoot?.querySelector('svg')).map(icon => icon.name);
  elements.forEach(icon => icon.remove());
  return missing;
}, icons);
if (missingIcons.length) errors.push(`wa-icon failed to resolve: ${missingIcons.join(', ')}`);

const formValues = await page.$eval('#rig-form', form => Object.fromEntries(new FormData(form)));
if (formValues.input_value !== 'hello') errors.push('wa-input missing from FormData');
if (formValues.avatar_key !== 'otter') errors.push('wa-radio-group missing from FormData');

for (const theme of themes) {
  await page.evaluate(name => cyclopusTheme.apply(name), theme);
  await page.waitForTimeout(200);
  const result = await page.evaluate(name => {
    const root = getComputedStyle(document.documentElement);
    const button = document.querySelector('#wa-rig wa-button[variant="brand"]');
    const buttonPart = button.shadowRoot.querySelector('[part~="button"]');
    const card = document.querySelector('#wa-rig');
    const inputPart = document.querySelector('#wa-rig wa-input').shadowRoot.querySelector('[part~="base"]');
    return {
      name,
      mode: document.documentElement.dataset.mode,
      waDark: document.documentElement.classList.contains('wa-dark'),
      token: root.getPropertyValue('--wa-color-brand-fill-loud').trim(),
      button: getComputedStyle(buttonPart).backgroundColor,
      buttonHeight: getComputedStyle(buttonPart).height,
      cardBackground: getComputedStyle(card).backgroundColor,
      cardBorder: getComputedStyle(card).borderColor,
      cardRadius: getComputedStyle(card).borderRadius,
      cardShadow: getComputedStyle(card).boxShadow,
      inputBackground: getComputedStyle(inputPart).backgroundColor,
      inputBorder: getComputedStyle(inputPart).borderColor,
      accent: root.getPropertyValue('--color-accent').trim(),
      info: root.getPropertyValue('--color-info').trim(),
      surface: root.getPropertyValue('--wa-color-surface-default').trim(),
      base: root.getPropertyValue('--cyclopus-base').trim(),
      panel: root.getPropertyValue('--cyclopus-panel').trim(),
    };
  }, theme);
  if (!result.button || result.button === 'rgba(0, 0, 0, 0)') {
    errors.push(`${theme}: brand button has no background`);
  }
  const shouldBeDark = !['paper', 'console', 'solar'].includes(theme);
  if (result.waDark !== shouldBeDark) errors.push(`${theme}: wrong wa-dark state`);
  const expected = await page.evaluate(color => {
    const node = document.createElement('span');
    node.style.color = color;
    document.body.appendChild(node);
    const normalized = getComputedStyle(node).color;
    node.remove();
    return normalized;
  }, result.token);
  if (result.button !== expected) errors.push(`${theme}: button ${result.button} != brand ${expected}`);
  if (result.buttonHeight !== '32px') errors.push(`${theme}: default button height ${result.buttonHeight} != 32px`);
  const expectedSurface = await page.evaluate(color => {
    const node = document.createElement('span'); node.style.color = color; document.body.appendChild(node);
    const normalized = getComputedStyle(node).color; node.remove(); return normalized;
  }, result.panel);
  const borderToken = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--wa-color-surface-border').trim());
  const expectedBorder = await page.evaluate(color => {
    const node = document.createElement('span'); node.style.color = color; document.body.appendChild(node);
    const normalized = getComputedStyle(node).color; node.remove(); return normalized;
  }, borderToken);
  const expectedRadius = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--wa-border-radius-l').trim());
  if (result.cardBackground !== expectedSurface) errors.push(`${theme}: card surface ${result.cardBackground} != ${expectedSurface}`);
  if (result.cardBorder !== expectedBorder) errors.push(`${theme}: card border ${result.cardBorder} != ${expectedBorder}`);
  if (result.cardRadius !== expectedRadius) errors.push(`${theme}: card radius ${result.cardRadius} != ${expectedRadius}`);
  if (result.cardShadow !== 'none') errors.push(`${theme}: card shadow should be none, got ${result.cardShadow}`);
}

for (const theme of ['ember', 'paper']) {
  await page.evaluate(name => cyclopusTheme.apply(name), theme);
  await page.waitForTimeout(200);
  const layers = await page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const style = element => {
      const value = getComputedStyle(element);
      return {
        background: value.backgroundColor,
        borderColor: value.borderTopColor,
        borderWidth: value.borderTopWidth,
        width: value.width,
        padding: value.padding,
      };
    };
    const part = (selector, name) => document.querySelector(selector).shadowRoot.querySelector(`[part~="${name}"]`);
    return {
      tokens: {
        panel: root.getPropertyValue('--cyclopus-panel').trim(),
        raise: root.getPropertyValue('--cyclopus-raise').trim(),
        strong: root.getPropertyValue('--cyclopus-line-strong').trim(),
      },
      neutral: style(part('#rig-neutral', 'button')),
      danger: style(part('#rig-danger', 'button')),
      avatar: style(part('#rig-avatar', 'button')),
      badge: style(document.querySelector('#wa-rig wa-badge')),
      badgeBody: style(part('#wa-rig wa-badge', 'base')),
      callout: style(document.querySelector('#rig-callout')),
      icon: style(document.querySelector('#rig-icon')),
      tooltip: style(part('#wa-rig wa-tooltip', 'body')),
    };
  });
  const normalize = async color => page.evaluate(value => {
    const node = document.createElement('span');
    node.style.color = value;
    document.body.appendChild(node);
    const normalized = getComputedStyle(node).color;
    node.remove();
    return normalized;
  }, color);
  const panel = await normalize(layers.tokens.panel);
  const raise = await normalize(layers.tokens.raise);
  const strong = await normalize(layers.tokens.strong);
  if (layers.neutral.background !== raise) errors.push(`${theme}: neutral button surface ${layers.neutral.background} != raise ${raise}`);
  if (layers.neutral.borderColor !== strong || layers.neutral.borderWidth !== '1px') errors.push(`${theme}: neutral button is not a single strong hairline`);
  if (layers.danger.background !== 'rgba(0, 0, 0, 0)') errors.push(`${theme}: danger button should be transparent`);
  if (layers.avatar.width !== '64px') errors.push(`${theme}: avatar button width ${layers.avatar.width} != 64px`);
  if (layers.badge.background !== 'rgba(0, 0, 0, 0)') errors.push(`${theme}: badge should be a transparent chip`);
  if (layers.badgeBody.borderWidth !== '0px' || layers.badgeBody.background !== 'rgba(0, 0, 0, 0)') errors.push(`${theme}: badge inner body draws a second layer`);
  if (layers.callout.padding !== '12px 16px') errors.push(`${theme}: callout host padding ${layers.callout.padding} != 12px 16px`);
  if (layers.icon.width !== '16px') errors.push(`${theme}: w-4 icon canvas ${layers.icon.width} != 16px`);
  if (layers.tooltip.background !== panel || layers.tooltip.borderColor !== strong) errors.push(`${theme}: tooltip is not panel/strong overlay`);
}

const input = page.locator('#wa-rig wa-input');
const focusedInput = await input.evaluate(element => {
  element.shadowRoot.querySelector('input').focus();
  const base = element.shadowRoot.querySelector('[part~="base"]');
  const style = getComputedStyle(base);
  return { border: style.borderColor, ring: style.boxShadow };
});
const focusColor = await page.evaluate(() => {
  const value = getComputedStyle(document.documentElement).getPropertyValue('--wa-color-focus').trim();
  const node = document.createElement('span'); node.style.color = value; document.body.appendChild(node);
  const normalized = getComputedStyle(node).color; node.remove(); return normalized;
});
if (focusedInput.ring === 'none') errors.push('input focus stroke is missing');

const themeRow = await page.locator('.theme-option').first().evaluate(element => {
  const style = getComputedStyle(element);
  return { display: style.display, columns: style.gridTemplateColumns };
});
if (themeRow.display !== 'flex') errors.push('theme choice is not using the source card layout');

await page.click('#rig-dialog-open');
if (!(await page.$eval('#rig-dialog', element => element.open))) errors.push('dialog did not open');
const dialogSurface = await page.$eval('#rig-dialog', element => {
  const dialog = element.shadowRoot.querySelector('[part="dialog"]');
  return getComputedStyle(dialog).backgroundColor;
});
const currentSurface = await page.evaluate(() => {
  const node = document.createElement('span');
  node.style.color = getComputedStyle(document.documentElement).getPropertyValue('--cyclopus-panel');
  document.body.appendChild(node);
  const value = getComputedStyle(node).color;
  node.remove();
  return value;
});
if (dialogSurface !== currentSurface) errors.push(`dialog surface ${dialogSurface} != panel ${currentSurface}`);
await page.keyboard.press('Escape');
await page.waitForFunction(() => !document.querySelector('#rig-dialog').open);

await page.click('#rig-dropdown wa-button');
if (!(await page.$eval('#rig-dropdown', element => element.open))) errors.push('dropdown did not open');
await page.keyboard.press('ArrowDown');
await page.keyboard.press('Enter');
await page.waitForFunction(() => !document.querySelector('#rig-dropdown').open);

await page.evaluate(async () => {
  await document.querySelector('#wa-rig wa-toast').create('Smoke test', { variant: 'success', duration: 1000 });
});
if ((await page.locator('wa-toast-item').count()) === 0) errors.push('toast did not appear');
const toastLayer = await page.locator('wa-toast-item').first().evaluate(element => {
  const part = element.shadowRoot.querySelector('[part~="toast-item"]');
  const style = getComputedStyle(part);
  const root = getComputedStyle(document.documentElement);
  return {
    background: style.backgroundColor,
    borderColor: style.borderTopColor,
    borderWidth: style.borderTopWidth,
    radius: style.borderRadius,
    panel: root.getPropertyValue('--cyclopus-panel').trim(),
    strong: root.getPropertyValue('--cyclopus-line-strong').trim(),
    cardRadius: root.getPropertyValue('--cyclopus-card-radius').trim(),
  };
});
const normalizeToastColor = async color => page.evaluate(value => {
  const node = document.createElement('span'); node.style.color = value; document.body.appendChild(node);
  const normalized = getComputedStyle(node).color; node.remove(); return normalized;
}, color);
if (toastLayer.background !== await normalizeToastColor(toastLayer.panel)) errors.push('toast surface is not panel');
if (toastLayer.borderColor !== await normalizeToastColor(toastLayer.strong) || toastLayer.borderWidth !== '1px') errors.push('toast is not a single strong hairline');
if (toastLayer.radius !== toastLayer.cardRadius) errors.push(`toast radius ${toastLayer.radius} != card radius ${toastLayer.cardRadius}`);

const beforeBoost = await page.locator('wa-toast-item').count();
await page.click('#rig-flash-load');
await page.waitForFunction(count => document.querySelectorAll('wa-toast-item').length > count, beforeBoost);

await browser.close();
if (errors.length) {
  console.error(errors.join('\n'));
  process.exit(1);
}
console.log('Web Awesome UI smoke test passed across all 8 themes.');
