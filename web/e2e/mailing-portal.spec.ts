import { test, expect } from '@playwright/test';

test.describe('Mailing Portal', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    // Wait for auth check to complete (dev mode bypasses auth)
    await page.waitForSelector('h1:has-text("Ignite Email Monitoring Portal")', { timeout: 10000 });
    // Navigate to Mailing portal
    await page.click('.nav-item:has-text("Mailing")');
    // Wait for mailing portal to load - look for the mailing-portal container
    await page.waitForSelector('.mailing-portal', { timeout: 5000 });
  });

  test('should display IGNITE Mailing header', async ({ page }) => {
    await expect(page.locator('.sidebar-header h1')).toContainText('IGNITE');
    await expect(page.locator('.sidebar-header .subtitle')).toContainText('Mailing Platform');
  });

  test('should show mailing navigation tabs', async ({ page }) => {
    const tabs = ['Dashboard', 'Campaigns', 'Lists', 'AI Plans', 'Servers', 'Templates'];
    for (const tab of tabs) {
      await expect(page.locator(`button.nav-item:has-text("${tab}")`)).toBeVisible();
    }
  });

  test('should display Dashboard by default', async ({ page }) => {
    // Dashboard should show Mailing Dashboard header
    await expect(page.locator('h1:has-text("Mailing Dashboard")')).toBeVisible();
  });

  test('should show dashboard stats', async ({ page }) => {
    await expect(page.locator('text=Total Subscribers')).toBeVisible();
    await expect(page.locator('text=Total Lists')).toBeVisible();
  });

  test('should show quick stats in sidebar', async ({ page }) => {
    await expect(page.locator('.quick-stats')).toBeVisible();
    await expect(page.locator('.quick-stat-label:has-text("Daily Capacity")')).toBeVisible();
  });

  test('should navigate to Campaigns tab', async ({ page }) => {
    await page.click('button.nav-item:has-text("Campaigns")');
    await expect(page.locator('.campaigns-header h1')).toContainText('Campaigns');
  });

  test('should navigate to Lists tab', async ({ page }) => {
    await page.click('button.nav-item:has-text("Lists")');
    await expect(page.locator('.lists-header h1')).toContainText('Mailing Lists');
  });

  test('should navigate to AI Plans tab', async ({ page }) => {
    await page.click('button.nav-item:has-text("AI Plans")');
    await expect(page.locator('text=AI Sending Plan')).toBeVisible();
  });

  test('should show sending plan options', async ({ page }) => {
    await page.click('button.nav-item:has-text("AI Plans")');
    await expect(page.locator('text=Morning Focus')).toBeVisible();
    await expect(page.locator('text=First Half Balanced')).toBeVisible();
    await expect(page.locator('text=Full Day Maximum')).toBeVisible();
  });

  test('should navigate to Servers placeholder', async ({ page }) => {
    await page.click('button.nav-item:has-text("Servers")');
    await expect(page.locator('h2:has-text("Delivery Servers")')).toBeVisible();
    await expect(page.locator('text=Coming Soon')).toBeVisible();
  });

  test('should navigate to Templates placeholder', async ({ page }) => {
    await page.click('button.nav-item:has-text("Templates")');
    await expect(page.locator('h2:has-text("Email Templates")')).toBeVisible();
    await expect(page.locator('text=Coming Soon')).toBeVisible();
  });

  test('should have back to analytics button', async ({ page }) => {
    await expect(page.locator('button:has-text("Back to Analytics")')).toBeVisible();
  });
});

test.describe('Mailing Portal - Lists Management', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('h1:has-text("Ignite Email Monitoring Portal")', { timeout: 10000 });
    await page.click('.nav-item:has-text("Mailing")');
    await page.waitForSelector('.mailing-portal', { timeout: 5000 });
    await page.click('button.nav-item:has-text("Lists")');
  });

  test('should display lists header', async ({ page }) => {
    await expect(page.locator('.lists-header h1')).toContainText('Mailing Lists');
  });

  test('should have create list button', async ({ page }) => {
    await expect(page.locator('button:has-text("Create List")')).toBeVisible();
  });
});

test.describe('Mailing Portal - Campaigns Management', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('h1:has-text("Ignite Email Monitoring Portal")', { timeout: 10000 });
    await page.click('.nav-item:has-text("Mailing")');
    await page.waitForSelector('.mailing-portal', { timeout: 5000 });
    await page.click('button.nav-item:has-text("Campaigns")');
  });

  test('should display campaigns header', async ({ page }) => {
    await expect(page.locator('.campaigns-header h1')).toContainText('Campaigns');
  });

  test('should have create campaign button', async ({ page }) => {
    await expect(page.locator('button:has-text("Create Campaign")')).toBeVisible();
  });
});
