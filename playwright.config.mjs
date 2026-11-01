import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir:'./tests',
  outputDir:'test-output/playwright',
  workers:1,
  reporter:'list',
  use:{baseURL:'http://127.0.0.1:28931',browserName:'chromium',channel:'chrome',headless:true},
  webServer:{command:'.\\test-output\\demo.exe',url:'http://127.0.0.1:28931/v0/resource/plugins/cpa-codex-candy-guard/ui',reuseExistingServer:false,timeout:30000}
});
