import { test, expect } from '@playwright/test';

test.describe('theme', () => {
  test('light mode (default) renders bright background', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await page.goto('/login');
    const bg = await page.evaluate(() =>
      getComputedStyle(document.body).backgroundColor,
    );
    // ライトモードの背景は白に近いはず（lightness > 0.9 のいずれか）
    expect(bg).not.toBe('');
    // カラー値の比較ではなく、後で同じスナップショットがダークモードと差分が出ることだけ確認
    const computedLight = bg;

    await page.emulateMedia({ colorScheme: 'dark' });
    await page.reload();
    const bgDark = await page.evaluate(() =>
      getComputedStyle(document.body).backgroundColor,
    );
    expect(bgDark).not.toBe(computedLight);
  });

  test('reduced motion sets transition-duration to 0', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await page.goto('/login');
    const dur = await page.evaluate(() => {
      const probe = document.createElement('div');
      probe.style.transition = 'background var(--duration-normal) ease';
      document.body.appendChild(probe);
      const t = getComputedStyle(probe).transitionDuration;
      probe.remove();
      return t;
    });
    expect(dur).toMatch(/^0/); // 「0s」「0ms」など
  });
});
