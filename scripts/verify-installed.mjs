import { chromium } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
const env = await readFile('D:/apps/cpa-usage-keeper/.env.docker','utf8');
const key = env.split(/\r?\n/).find(line=>line.startsWith('CPA_MANAGEMENT_KEY=')).split('=').slice(1).join('=').trim().replace(/^["']|["']$/g,'');
const browser = await chromium.launch({channel:'chrome',headless:true});
const context = await browser.newContext({viewport:{width:1280,height:900}});
const page = await context.newPage();
const errors=[];page.on('pageerror',error=>errors.push(error.message));
try {
  await page.goto('http://127.0.0.1:18317/v0/resource/plugins/cpa-codex-candy-guard/ui');
  await page.locator('#key').fill(key);await page.locator('#login button').click();
  await page.locator('#connection').filter({hasText:'已连接'}).waitFor();
  await page.locator('#accounts input[data-id]').first().waitFor();
  if (await page.locator('#scheduled').isChecked()) throw new Error('Schedule must remain off');
  await page.screenshot({path:'test-output/installed-desktop.png',fullPage:false,mask:[page.locator('.account-name')]});
  const accounts=await page.locator('#accounts input[data-id]').count();
  const models=await page.locator('#model option').count()-1;
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({path:'test-output/installed-mobile.png',fullPage:false,mask:[page.locator('.account-name')]});
  const layout=await page.evaluate(()=>({width:innerWidth,scroll:document.documentElement.scrollWidth}));
  if(layout.scroll>layout.width||errors.length)throw new Error('Layout or runtime validation failed');
  const result={accounts,models,schedule_enabled:false,browser_errors:errors,desktop:'installed-desktop.png',mobile:'installed-mobile.png',read_only_verification:true};
  await writeFile('test-output/installed-browser.json',JSON.stringify(result,null,2));
  console.log(JSON.stringify(result));
}finally{await context.close();await browser.close();}
