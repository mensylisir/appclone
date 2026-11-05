// 最终全量回归（22 项）：native click + 真实 focusout 重命名路径
const pp = require('/home/linuxbrew/.linuxbrew/lib/node_modules/puppeteer-core').default;
(async () => {
  const browser = await pp.launch({executablePath:'/usr/bin/chromium-browser', args:['--no-sandbox','--disable-gpu','--headless=new']});
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.setViewport({width:1280, height:1000});
  await page.goto('file:///home/mensyli1/Documents/workspace/appclone/design/appclone-gui.html');
  await new Promise(r=>setTimeout(r,500));

  let pass = 0, fail = 0;
  const t = async (name, fn) => {
    try { const r = await fn(); if (r === true) { pass++; console.log('✅ ' + name); } else { fail++; console.log('❌ ' + name + ' → ' + r); } }
    catch(e){ fail++; console.log('❌ ' + name + ' → 异常: ' + e.message.split('\n')[0]); }
  };
  const click = sel => page.evaluate(s => document.querySelector(s).click(), sel);
  const ev = (fn, ...args) => page.evaluate(fn, ...args);

  await t('Tab 切屏2', async () => { await click('[data-scr="2"]');
    return ev(() => !document.querySelector('#scr1').offsetParent && !!document.querySelector('#scr2').offsetParent); });
  await t('返回按钮回屏1', async () => { await click('#btnBack');
    return ev(() => !!document.querySelector('#scr1').offsetParent && !document.querySelector('#scr2').offsetParent); });
  await t('行点击进详情(微信)', async () => { await click('[data-app="wechat"]');
    return ev(() => !!document.querySelector('#scr2').offsetParent && document.querySelector('#hbTitle').textContent === '微信'); });
  await t('启动按钮 → Toast', async () => { await click('[data-launch="0"]');
    return ev(() => document.querySelector('#toast').classList.contains('show')); });
  await t('删除实例', async () => { await click('[data-del="0"]');
    return ev(() => document.querySelector('#instBox').textContent.includes('还没有分身实例')); });
  await t('新建分身 → 对话框(clone 模式)', async () => { await click('#rowAddInst');
    return ev(() => document.querySelector('#overlay').classList.contains('open')
              && document.querySelector('#dlgTitle').textContent.includes('新建分身')); });
  await t('Esc 关闭对话框', async () => { await page.keyboard.press('Escape');
    return ev(() => !document.querySelector('#overlay').classList.contains('open')); });
  await t('屏3 Tab 直接打开对话框', async () => { await click('[data-scr="3"]');
    return ev(() => document.querySelector('#overlay').classList.contains('open')); });
  await t('Tab 高亮状态正确', async () => {
    return ev(() => document.querySelector('[data-scr="3"]').getAttribute('aria-selected') === 'true'); });
  await t('全部取消勾选 → 确认禁用', async () => {
    return ev(() => {
      document.querySelectorAll('[data-chk]').forEach(c => { if(!c.disabled && c.checked) c.click(); });
      return document.querySelector('#dlgConfirm').disabled;
    });
  });
  await t('恢复勾选 → 确认可用', async () => {
    return ev(() => { document.querySelectorAll('[data-chk]').forEach(c => { if(!c.disabled && !c.checked) c.click(); });
      return !document.querySelector('#dlgConfirm').disabled; }); });
  await t('对话框确认 → Toast + 关闭', async () => { await click('#dlgConfirm');
    return ev(() => !document.querySelector('#overlay').classList.contains('open')
              && document.querySelector('#toast').classList.contains('show')); });
  await t('返回 → 配置钉钉(首次配置模式)', async () => { await click('#btnBack'); await click('[data-setup="dingtalk"]');
    return ev(() => document.querySelector('#overlay').classList.contains('open')
              && document.querySelector('#dlgTitle').textContent.includes('首次配置')); });
  await t('首次配置确认 → 钉钉入已支持组', async () => { await click('#dlgConfirm');
    return ev(() => !!document.querySelector('[data-app="dingtalk"]')); });
  await t('深色主题切换', async () => { await click('#btnTheme');
    return ev(() => document.documentElement.classList.contains('dark')); });
  await t('空态演示开', async () => { await click('#btnTheme'); await click('#btnEmpty');
    return ev(() => document.querySelector('#emptyState').style.display !== 'none'); });
  await t('空态演示还原', async () => { await click('#btnEmpty');
    return ev(() => document.querySelector('#emptyState').style.display === 'none'); });
  await t('横幅演示 + 知道了关闭', async () => { await click('#btnBanner'); const a = await ev(() => document.querySelector('#banner').classList.contains('show'));
    await click('#bannerDismiss'); const b = await ev(() => !document.querySelector('#banner').classList.contains('show')); return a && b; });
  await t('开关开(qq) → 副标题更新', async () => { await click('[data-sw="qq"]');
    return ev(() => { const s=document.querySelector('[data-sw="qq"]'); return s.checked && s.closest('.row').querySelector('[data-sub]').textContent.includes('1 个分身'); }); });
  await t('开关关(qq) → 副标题未开启', async () => { await click('[data-sw="qq"]');
    return ev(() => { const s=document.querySelector('[data-sw="qq"]'); return !s.checked && s.closest('.row').querySelector('[data-sub]').textContent.includes('未开启'); }); });
  await t('qq 开分身 → 详情重命名(真实 focusout)', async () => {
    await click('[data-sw="qq"]'); await new Promise(r=>setTimeout(r,2700));
    await click('[data-app="qq"]');
    await page.click('.iname');
    await page.evaluate(() => { document.querySelector('.iname').textContent = '工作号'; });
    await page.click('#hbTitle');
    await new Promise(r=>setTimeout(r,200));
    return ev(() => document.querySelector('#toast').textContent.includes('已重命名为「工作号」')); });
  await t('重命名后实例名保持', async () => {
    return ev(() => document.querySelector('.iname')?.textContent === '工作号'); });

  console.log(`\n结果: ${pass} 通过 / ${fail} 失败`);
  console.log(errors.length ? '❗JS错误:\n' + errors.join('\n') : '全程无页面 JS 错误');
  await browser.close();
})();
