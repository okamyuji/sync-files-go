import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { signupAndLogin } from '../fixtures/auth';

/**
 * axe-core で各画面のアクセシビリティ違反を検出する。
 * Critical / Serious 違反ゼロが Phase 5 の受け入れ基準。
 */
async function expectNoCriticalA11yViolations(page: any) {
  const result = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21aa'])
    .analyze();
  const blocking = result.violations.filter(
    v => v.impact === 'critical' || v.impact === 'serious',
  );
  if (blocking.length > 0) {
    console.log('axe violations:', JSON.stringify(blocking, null, 2));
  }
  expect(blocking, 'no critical/serious axe violations').toEqual([]);
}

test.describe('accessibility (axe-core)', () => {
  test('login page', async ({ page }) => {
    await page.goto('/login');
    await expectNoCriticalA11yViolations(page);
  });

  test('signup page', async ({ page }) => {
    await page.goto('/signup');
    await expectNoCriticalA11yViolations(page);
  });

  test('home page (authenticated)', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);
    await page.goto('/');
    await expectNoCriticalA11yViolations(page);
  });

  test('trash page (authenticated)', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);
    await page.goto('/trash');
    await expectNoCriticalA11yViolations(page);
  });

  test('share-links page (authenticated)', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);
    await page.goto('/share-links');
    await expectNoCriticalA11yViolations(page);
  });
});
