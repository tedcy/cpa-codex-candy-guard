import { test, expect } from '@playwright/test';

async function open(page) {
  await page.goto('/v0/resource/plugins/cpa-codex-candy-guard/ui');
  await page.locator('#key').fill('synthetic-ui-key');
  await page.locator('#login button').click();
  await expect(page.locator('#connection')).toHaveText('已连接');
  await expect(page.locator('#accounts tr')).toHaveCount(12);
}

test('desktop configuration, run and history',async({page})=>{
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.setViewportSize({width:1240,height:860});await open(page);
  await page.locator('#model').selectOption('gpt-6.1-sol');await page.locator('#per').fill('2');await page.locator('#global').fill('3');
  await page.getByRole('button',{name:'保存设置',exact:true}).click();await expect(page.locator('#notice')).toHaveText('设置已保存');
  await page.getByRole('button',{name:'立即检测',exact:true}).click();
  await page.getByRole('tab',{name:'运行记录',exact:true}).click();
  await expect(page.locator('#history details')).toHaveCount(1);
  await expect(page.locator('#history summary .badge')).toHaveText('完成',{timeout:15000});
  await expect(page.locator('#round-state')).toHaveText('待命');
  await expect(page.locator('#history summary')).toContainText('0 已关闭');
  await page.locator('#history summary').click();await page.screenshot({path:'test-output/ui-desktop.png',fullPage:true});
  expect(errors).toEqual([]);expect(await page.locator('button svg').count()).toBeGreaterThan(3);
});

test('mobile controls and account selection',async({page})=>{
  await page.setViewportSize({width:390,height:844});await open(page);
  await page.getByRole('tab',{name:'账号',exact:true}).click();
  await page.locator('#select-all').check();await expect(page.locator('#selected-count')).toHaveText('12');
  await page.getByRole('radio',{name:/已选账号/}).check();await page.locator('#model').selectOption('gpt-6.1-sol');
  await page.getByRole('button',{name:'保存设置',exact:true}).click();
  const layout=await page.evaluate(()=>({width:innerWidth,scroll:document.documentElement.scrollWidth,controls:[...document.querySelectorAll('.controls>*')].map(e=>{const r=e.getBoundingClientRect();return {left:r.left,right:r.right,bottom:r.bottom,top:r.top}})}));
  expect(layout.scroll).toBeLessThanOrEqual(layout.width);for(const r of layout.controls){expect(r.left).toBeGreaterThanOrEqual(0);expect(r.right).toBeLessThanOrEqual(layout.width);}
  await page.screenshot({path:'test-output/ui-mobile.png',fullPage:true});
});
