import { test, expect } from '@playwright/test';

test('debug cookie flow', async ({ page, context }) => {
  await page.goto('http://localhost:8080/login');
  const cookies = await context.cookies();
  console.log('Cookies after GET /login:', JSON.stringify(cookies, null, 2));
  const csrfInput = await page.locator('input[name="_csrf"]').getAttribute('value');
  console.log('Form _csrf value:', csrfInput);
});
