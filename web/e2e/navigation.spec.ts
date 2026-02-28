import { test, expect } from '@playwright/test';

test.describe('Navigation', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    // Wait for auth check to complete (dev mode bypasses auth)
    await page.waitForSelector('h1:has-text("Ignite Email Monitoring Portal")', { timeout: 10000 });
  });

  test('should load the dashboard', async ({ page }) => {
    await expect(page.locator('h1')).toContainText('Ignite Email Monitoring Portal');
  });

  test('should display all navigation tabs', async ({ page }) => {
    const navItems = [
      'Mailing',
      'SparkPost',
      'Mailgun',
      'AWS SES',
      'Ongage',
      'Revenue',
      'Contracts',
      'Financials',
      'Intelligence',
      'Planning'
    ];

    for (const item of navItems) {
      await expect(page.locator(`.nav-item:has-text("${item}")`)).toBeVisible();
    }
  });

  test('should navigate to SparkPost dashboard by default', async ({ page }) => {
    await expect(page.locator('h2')).toContainText('SparkPost Dashboard');
  });

  test('should show ISP performance metrics', async ({ page }) => {
    await expect(page.locator('text=Top Performing ISPs')).toBeVisible();
    await expect(page.locator('text=By Volume')).toBeVisible();
    await expect(page.locator('text=By CTR')).toBeVisible();
  });

  test('should display active alerts', async ({ page }) => {
    await expect(page.locator('h3:has-text("Active Alerts")')).toBeVisible();
  });

  test('should navigate to Mailing portal', async ({ page }) => {
    await page.click('.nav-item:has-text("Mailing")');
    // MailingPortal has h1 "📬 IGNITE" with subtitle "Mailing Platform"
    await expect(page.locator('.mailing-portal')).toBeVisible({ timeout: 5000 });
    await expect(page.locator('.sidebar-header h1')).toContainText('IGNITE');
  });

  test('should navigate to Mailgun dashboard', async ({ page }) => {
    await page.click('.nav-item:has-text("Mailgun")');
    await expect(page.locator('h2:has-text("Mailgun")')).toBeVisible({ timeout: 5000 });
  });

  test('should navigate to Financials', async ({ page }) => {
    await page.click('.nav-item:has-text("Financials")');
    await expect(page.locator('h2:has-text("Financials Access")')).toBeVisible({ timeout: 5000 });
  });
});
