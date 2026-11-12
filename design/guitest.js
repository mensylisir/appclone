// 交互回归（74 项）：三屏 + 探测四态（probing/confirm/creating/error）+ 五入口 + 三处破坏性确认
//                    + 门禁 + 搜索 + 横幅语义 + 空态可达性 + 运行/停止状态
//                    + 读证据四态/锁定缓存行/credential 徽标/confirm-empty
//                    + 登录状态（独立密钥环）：默认策略/隐含锁定/能力缺失/迁移提示
// 运行：node design/guitest.js        （退出码 0 = EXPECTED 全过，非 0 = 设计基线被破坏）
// 依赖：任一本机 Chromium + puppeteer-core；路径可用环境变量覆盖：
//   CHROMIUM=/path/to/chromium  PUPPETEER_CORE=/path/to/puppeteer-core
const path = require('path');
const fs = require('fs');
const { pathToFileURL } = require('url');

const EXPECTED = 74;   // 断言总数；改动断言必须同步改这里（否则退出码永远是 1）

function loadPuppeteer() {
  const cands = [
    process.env.PUPPETEER_CORE,
    'puppeteer-core',
    path.join(__dirname, '..', 'node_modules', 'puppeteer-core'),
    '/home/linuxbrew/.linuxbrew/lib/node_modules/puppeteer-core',
    '/usr/lib/node_modules/puppeteer-core',
    '/usr/local/lib/node_modules/puppeteer-core',
    path.join(process.env.HOME || '', '.npm-global/lib/node_modules/puppeteer-core'),
    // WorkBuddy 托管的 node 工作区（本机把依赖装在这里）
    path.join(process.env.HOME || '', '.workbuddy/binaries/node/workspace/node_modules/puppeteer-core'),
  ].filter(Boolean);
  for (const c of cands) { try { return require(c).default || require(c); } catch (_) {} }
  console.error('❌ 找不到 puppeteer-core。装一份或用 PUPPETEER_CORE=/path 指定。');
  process.exit(2);
}

function findChrome() {
  const cands = [
    process.env.CHROMIUM,
    '/usr/bin/chromium', '/usr/bin/chromium-browser',
    '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/snap/bin/chromium',
  ].filter(Boolean);
  for (const c of cands) if (fs.existsSync(c)) return c;
  console.error('❌ 找不到 Chromium。装了或用 CHROMIUM=/path 指定。');
  process.exit(2);
}

const pp = loadPuppeteer();
(async () => {
  const browser = await pp.launch({executablePath: findChrome(), args:['--no-sandbox','--disable-gpu','--headless=new']});
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.setViewport({width:1280, height:1000});
  await page.goto(pathToFileURL(path.join(__dirname, 'appclone-gui.html')).href);
  await new Promise(r=>setTimeout(r,500));

  let pass = 0, fail = 0;
  const t = async (name, fn) => {
    try { const r = await fn(); if (r === true) { pass++; console.log('✅ ' + name); } else { fail++; console.log('❌ ' + name + ' → ' + r); } }
    catch(e){ fail++; console.log('❌ ' + name + ' → 异常: ' + e.message.split('\n')[0]); }
  };
  const click = sel => page.evaluate(s => document.querySelector(s).click(), sel);
  /* 屏1 是两个平级视图（标题栏的 AdwViewSwitcher）：断言前先走到目标视图，
     不要靠程序化点击去点一个用户此刻根本看不见的开关 */
  const showView = v => click(`.vs-btn[data-view="${v}"]`);
  const ev = (fn, ...args) => page.evaluate(fn, ...args);
  // 探测是异步的（1.2s），断言前必须等状态落地，不能用固定 sleep 猜
  const waitFor = async (fn, timeout = 4000) => {
    const t0 = Date.now();
    while (Date.now() - t0 < timeout) {
      if (await page.evaluate(fn)) return true;
      await new Promise(r => setTimeout(r, 80));
    }
    return false;
  };
  const textOf = sel => ev(s => document.querySelector(s)?.textContent ?? '', sel);
  /* 确认是**异步**的（confirm → creating → 落详情页，400ms+）：点完就断言只能测到"还在创建中"，
     而且残留的创建回调会污染后续每一条断言——这种连锁失败最容易被误读成"实现坏了" */
  const confirmAndWait = async (sel = '#dlgConfirm') => {
    await click(sel);
    await waitFor(() => !document.querySelector('#overlay').classList.contains('open'), 6000);
  };
  /* 搜索有 120ms 防抖：逐键重建 397 行 DOM 会顿挫，所以断言必须等防抖窗口过去再读 */
  const setSearch = async v => {
    await ev(val => {
      const s = document.querySelector('#search'); s.value = val;
      s.dispatchEvent(new Event('input', {bubbles:true}));
    }, v);
    await new Promise(r => setTimeout(r, 220));
  };

  await t('Tab 切屏2', async () => { await click('[data-scr="2"]');
    return ev(() => !document.querySelector('#scr1').offsetParent && !!document.querySelector('#scr2').offsetParent); });
  await t('返回按钮回屏1', async () => { await click('#btnBack');
    return ev(() => !!document.querySelector('#scr1').offsetParent && !document.querySelector('#scr2').offsetParent); });
  /* 「分身」是第一落点（演示数据里有实例）：标题栏是视图切换器，不是标题文字 */
  await t('行点击进详情(微信)', async () => {
    const landed = await ev(() => !!document.querySelector('#viewSwitcher')?.offsetParent
      && !!document.querySelector('#viewClones')?.offsetParent
      && document.querySelector('.vs-btn[data-view="clones"]').getAttribute('aria-selected') === 'true'
      /* 角标在当前页也显示（libadwaita 1.10 实测），数字 = 已开启的应用数 */
      && !!document.querySelector('#vsBadge')?.offsetParent
      && document.querySelector('#vsBadge').textContent === '2');
    await click('[data-app="wechat"]');
    return (landed && await ev(() => !!document.querySelector('#scr2').offsetParent
      && document.querySelector('#hbTitle').textContent === '微信'))
      || '首页没有落在「分身」视图，或行点击没进详情'; });
  /* 用户可见文案里不能出现内部 id（work）与引擎名（bwrap）：id 是目录名，用户只认显示名。
     只断言"toast 出现"会漏掉这类泄漏，必须读文本 */
  await t('启动按钮 → Toast 且只出现显示名（不泄漏 id / bwrap）', async () => { await click('[data-launch="0"]');
    return ev(() => { const txt = document.querySelector('#toast').textContent;
      return document.querySelector('#toast').classList.contains('show')
        && txt.includes('工作号') && !txt.includes('work') && !txt.includes('bwrap'); })
      || '文案泄漏内部 id 或引擎名：' + await textOf('#toast'); });

  // —— 破坏性确认之一：删单个实例 ——
  await t('删除实例 → 弹确认（不直接删）', async () => { await click('[data-del="0"]');
    return ev(() => document.querySelector('#alertOverlay').classList.contains('open')
              && document.querySelector('#instBox').textContent.includes('工作号')); });
  /* default response 必须是安全那一个：把「移到回收站」设成默认 = 键盘用户回车即删，
     而对话框存在的意义正是"不要一键删" */
  await t('破坏性确认的默认响应是「取消」（回车不会删）', async () => {
    const isCancel = await ev(() => document.querySelector('#alertCancel').dataset.default === '1'
      && !document.querySelector('#alertOk').dataset.default);
    await page.keyboard.press('Enter');
    const alive = await ev(() => !document.querySelector('#alertOverlay').classList.contains('open')
      && document.querySelector('#instBox').textContent.includes('工作号'));
    return (isCancel && alive) || '默认响应不对，或回车把实例删了';
  });
  await t('删除确认 → 取消后实例仍在', async () => { await click('[data-del="0"]'); await click('#alertCancel');
    return ev(() => !document.querySelector('#alertOverlay').classList.contains('open')
              && !document.querySelector('#instBox').textContent.includes('还没有分身实例')); });
  await t('删除确认 → 确认后实例消失', async () => { await click('[data-del="0"]'); await click('#alertOk');
    return ev(() => document.querySelector('#instBox').textContent.includes('还没有分身实例')); });

  // 详情页的「＋新建分身」= 再要一个（clone），与列表开关的「开启分身」（open）必须分开说
  await t('新建分身 → 对话框标题是「新建分身」，不是「开启分身」', async () => { await click('#rowAddInst');
    return ev(() => { const d = document.querySelector('#dlgTitle').textContent;
      return document.querySelector('#overlay').classList.contains('open')
        && d.includes('新建分身') && !d.includes('首次配置')
        && document.querySelector('#dlgConfirm').textContent === '创建分身'; }); });
  await t('Esc 关闭对话框', async () => { await page.keyboard.press('Escape');
    return ev(() => !document.querySelector('#overlay').classList.contains('open')); });

  // —— 屏3 状态机：probing → confirm ——
  await t('屏3 Tab 打开 → 探测中态（spinner + 取消可用 + 无创建按钮）', async () => {
    await click('[data-scr="3"]');
    return ev(() => document.querySelector('#overlay').classList.contains('open')
              && !!document.querySelector('.spinner')
              && document.querySelector('#dlgCancel').textContent.includes('取消探测')
              && document.querySelector('#dlgConfirm').style.display === 'none'); });
  /* overlay 只知道"写过/删过"，拿不到写入次数。文案写「命中 N 次」就是承诺了 core 给不出的数据 */
  await t('探测中的计数措辞是「写入条目」而不是「命中次数」', async () => {
    return ev(() => {
      const txt = document.querySelector('#dlgBody').textContent;
      return txt.includes('个写入条目') && !txt.includes('命中');
    }); });
  await t('探测完成 → 进入确认态，按钮是「创建分身」', async () => {
    const ok = await waitFor(() => !!document.querySelector('[data-chk]'));
    if (!ok) return '探测没在上限内进入 confirm 态';
    const label = await textOf('#dlgConfirm');
    return label === '创建分身' || 'setup 入口的确认按钮应为「创建分身」，实际：' + label; });
  /* 只断言"3 号是选中"是弱断言：三个 tab 同时选中也会通过。
     要的是"有且只有一个选中，且是 3" */
  await t('Tab 高亮状态正确（有且只有一个选中，且是屏3）', async () => {
    return ev(() => {
      const sel = [...document.querySelectorAll('.tab')].filter(t => t.getAttribute('aria-selected') === 'true');
      return sel.length === 1 && sel[0].dataset.scr === '3';
    }); });
  await t('公共目录组存在且只读（不在可勾列表里）', async () => {
    return ev(() => {
      const txt = document.querySelector('#dlgBody').textContent;
      const ro = [...document.querySelectorAll('.readonly-row')]
        .filter(r => r.textContent.includes('始终共享')).length;
      const checkedPaths = [...document.querySelectorAll('.chkrow .path')].map(e => e.textContent).join(',');
      return txt.includes('公共目录（强制共享）') && ro === 8 && !checkedPaths.includes('/Downloads');
    }); });
  /* 候选行只给路径不给"被写了多少"，用户无从判断哪条值得勾；
     但也不能写"命中 N 次"——overlay 拿不到次数 */
  await t('确认页每个候选显示「N 个写入条目」', async () => {
    return ev(() => {
      /* 「单例处理」「登录状态」两行也是 .chkrow，但它们不是观测出来的路径，
         不该有写入条目数 */
      const rows = [...document.querySelectorAll('.chkrow')].filter(r => {
        const inp = r.querySelector('[data-chk]');
        return inp && inp.dataset.kind !== 'singleton' && inp.dataset.kind !== 'keyring';
      });
      return rows.length > 0 && rows.every(r => /(\d+ 个写入条目|\d+ 条读取路径|\d+ 个写入条目 · \d+ 条读取路径)/.test(r.textContent))
        && !document.querySelector('#dlgBody').textContent.includes('命中');
    }); });
  await t('登录态行带「登录态」/「包含登录态」标记', async () => {
    return ev(() => {
      const txt = document.querySelector('#dlgBody').textContent;
      return !!document.querySelector('[data-credential="strong"]')
        && !!document.querySelector('[data-contains-credential]')
        && txt.includes('登录态');
    }); });
  await t('超过 20 条的读取路径显示「+N 未显示」', async () => {
    return ev(() => {
      const el = document.querySelector('#reads-hidden');
      return !!el && /^\+\d+ 条读取路径未显示$/.test(el.textContent);
    }); });
  await t('读证据不完整 → partial 横幅常驻确认页', async () => {
    return ev(() => {
      const el = document.querySelector('[data-banner="reads-partial"]');
      return !!el && el.offsetParent !== null
        && el.textContent.includes('登录态可能未覆盖');
    });
  });
  /* 第五组「共享但可写」：档案字段 shared_writes 的界面可见性。
     代价（两个分身的最近文件会混）不写出来，用户遇到就判定产品坏了 */
  await t('共享但可写组存在、只读、且写清代价', async () => {
    return ev(() => {
      const txt = document.querySelector('#dlgBody').textContent;
      const chkPaths = [...document.querySelectorAll('.chkrow .path')].map(e => e.textContent).join(',');
      return txt.includes('共享但可写（不隔离）')
        && txt.includes('最近文件')
        && txt.includes('输入法')
        && !chkPaths.includes('recently-used.xbel');   // 只读：不在可勾列表里
    }); });
  /* 登录状态（ADR-035）：观测到使用系统密钥环 → 默认开。开启即隐含私有总线与
     runtime，单例行同步点亮并锁住，界面状态与 bwrap 实际行为一致 */
  await t('登录状态组始终出现：观测到使用 → 默认开启并锁住单例行', async () => {
    return ev(() => {
      const g = document.querySelector('[data-group="keyring"]');
      const kr = document.querySelector('[data-chk][data-kind="keyring"]');
      const sg = [...document.querySelectorAll('[data-chk][data-kind="singleton"]')];
      return !!g && !!kr && kr.checked
        && sg.length > 0 && sg.every(c => c.checked && c.disabled)
        && g.textContent.includes('已检测到该应用使用系统密钥环')
        && !document.querySelector('#dlgConfirm').disabled;
    }) || '登录状态组缺失、默认没开、或单例行没被锁住';
  });
  await t('关闭独立密钥环 → 单例行恢复可操作，门禁不受影响', async () => {
    return ev(() => {
      const kr = document.querySelector('[data-chk][data-kind="keyring"]');
      kr.click();
      const sg = [...document.querySelectorAll('[data-chk][data-kind="singleton"]')];
      return !kr.checked && sg.every(c => !c.disabled)
        && !document.querySelector('#dlgConfirm').disabled;
    }) || '关闭后单例行仍被锁，或确认按钮被误禁用';
  });
  await t('重新开启独立密钥环 → 恢复锁定', async () => {
    return ev(() => {
      const kr = document.querySelector('[data-chk][data-kind="keyring"]');
      kr.click();
      const sg = [...document.querySelectorAll('[data-chk][data-kind="singleton"]')];
      return kr.checked && sg.every(c => c.checked && c.disabled);
    }) || '重新开启后没有恢复锁定';
  });
  await t('全部取消勾选 → 确认禁用', async () => {
    return ev(() => {
      document.querySelectorAll('[data-chk]').forEach(c => { if (c.checked) c.click(); });
      return document.querySelector('#dlgConfirm').disabled;
    });
  });
  await t('缓存行锁定、无 checkbox，门禁不受其影响', async () => {
    return ev(() => {
      const locked = document.querySelector('.chkrow[data-locked] .chip.cache');
      const cacheInputs = document.querySelectorAll('[data-chk][data-kind="cache"]');
      return !!locked && cacheInputs.length === 0
        && document.querySelector('#dlgConfirm').disabled;
    });
  });
  await t('恢复勾选 → 确认可用', async () => {
    return ev(() => { document.querySelectorAll('[data-chk]').forEach(c => { if(!c.checked) c.click(); });
      return !document.querySelector('#dlgConfirm').disabled; }); });
  /* 数据来源（方案设计 §8.1）：默认关（与 CLI 不带 --fork 一致），用户显式开启才
     从现有数据克隆；只有建实例的入口出现，重新探测没有它 */
  await t('数据来源 → 克隆起步开关默认关、可开启', async () => {
    const r = await ev(() => {
      const f = document.querySelector('[data-fork]');
      if (!f) return 'missing';
      if (f.checked) return 'default-on';
      f.click();
      return f.checked && document.querySelector('#dlgBody').textContent.includes('写时复制');
    });
    return r === true || '克隆起步开关异常：' + r;
  });
  /* creating 是独立第四态：写档案 + 建目录 + 生成 .desktop 几百毫秒，
     期间再点一次确认就会建出两个实例，而中途取消会留下"档案写了、实例没建成"的半成品 */
  await t('确认 → creating 态（按钮「正在创建…」、确认与取消都禁用）', async () => {
    await click('#dlgConfirm');
    const busy = await ev(() => document.querySelector('#overlay').classList.contains('open')
      && document.querySelector('#dlgConfirm').textContent === '正在创建…'
      && document.querySelector('#dlgConfirm').disabled
      && document.querySelector('#dlgCancel').disabled);
    return busy || '没进 creating 态：' + await textOf('#dlgConfirm');
  });
  /* creating 的契约是"事务已经开始，不能半途停下"——三条关闭路径都要被拒：
     Esc、取消按钮、点遮罩。少拦一条就会留下"档案写了、实例没建成"的半成品 */
  await t('creating 期间 Esc / 取消 / 点遮罩都关不掉', async () => {
    await page.keyboard.press('Escape');
    await ev(() => {
      document.querySelector('#dlgCancel').dispatchEvent(new MouseEvent('click', {bubbles:true}));
      document.querySelector('#overlay').dispatchEvent(new MouseEvent('click', {bubbles:true}));
    });
    return ev(() => document.querySelector('#overlay').classList.contains('open'))
      || 'creating 期间对话框被关掉了（会留半成品）';
  });
  /* 断言的是**不变式**（再提交一次也只建一个实例），不绑定某一种实现手段：
     真实交互里按钮已 disabled，合成事件仍能到达事件层，所以这里故意绕开 disabled 派发一次点击 */
  await t('creating 期间重复提交 → 仍只建一个实例', async () => {
    await ev(() => document.querySelector('#dlgConfirm')
      .dispatchEvent(new MouseEvent('click', {bubbles:true})));
    await waitFor(() => !document.querySelector('#overlay').classList.contains('open'), 6000);
    await new Promise(r => setTimeout(r, 600));   // 熬过可能存在的第二个事务回调
    return ev(() => document.querySelectorAll('.iname').length === 1)
      || '建出了不止一个实例';
  });
  await t('对话框确认 → Toast + 关闭', async () => {
    return ev(() => !document.querySelector('#overlay').classList.contains('open')
              && document.querySelector('#toast').classList.contains('show')); });
  await t('首次配置完成 → replace 成详情页（不是回列表）', async () => {
    return ev(() => !!document.querySelector('#scr2').offsetParent
              && document.querySelector('#hbTitle').textContent === '钉钉'
              && document.querySelector('#instBox').textContent.includes('分身 1')); });

  await t('开启克隆起步后创建的实例带 fork 标记', async () => {
    return ev(() => {
      const a = apps.find(x => x.name === '钉钉');
      return !!a && a.instances.length === 1 && a.instances[0].fork === true;
    });
  });

  // —— 屏1 搜索（397 个应用的入场券）——
  await t('搜索过滤「微」→ 未匹配的从所有应用组消失', async () => {
    await click('#btnBack');
    await showView('all');                 // 搜索框只属于「所有应用」视图
    await setSearch('微');
    return ev(() => {
      const unc = document.querySelector('#unconfBox').textContent;
      /* 微信此时 0 实例（前面删过），应在"所有应用"组里且是唯一匹配 */
      return unc.includes('微信') && !unc.includes('Chrome') && !unc.includes('Telegram');
    }); });
  await t('清空搜索 → 列表恢复', async () => {
    await setSearch('');
    return ev(() => document.querySelector('#unconfBox').textContent.includes('Chrome'));
  });

  await t('深色主题切换', async () => { await click('#btnTheme');
    return ev(() => document.documentElement.classList.contains('dark')); });
  /* 空态只取代「分身」视图的列表，**另一个视图必须可达**：
     空态里的「选择应用」与标题栏的视图切换器是两条路——都断了就是产品首启死路 */
  await t('空态演示 → 空态显示，且「选择应用」通向可见的应用列表', async () => {
    await click('#btnTheme'); await click('#btnEmpty');
    const emptyOk = await ev(() => !!document.querySelector('#emptyState').offsetParent
      && !document.querySelector('#grpSupported').offsetParent
      && !!document.querySelector('#btnPickApps')?.offsetParent);
    if (!emptyOk) return '空态没有显示出来';
    await click('#btnPickApps');
    const listOk = await ev(() => !!document.querySelector('#viewAll').offsetParent
      && !!document.querySelector('#search')?.offsetParent          // 可见，不是"DOM 里有"
      && document.querySelectorAll('#unconfBox [data-sw]').length > 0);
    return listOk || '「选择应用」没有把用户带到可见的应用列表（首启无路可走）';
  });
  await t('空态下点亮开关 → 能走进首次配置（首启有路）', async () => {
    /* 光"DOM 里有这个开关"不够：整块列表被 display:none 时它照样能被程序点中，
       而真实用户看不见也点不到。必须断言它**可见**（offsetParent 非空） */
    if (!await ev(() => !!document.querySelector('#unconfBox [data-sw="slack"]')?.offsetParent))
      return '空态下开关不可见 —— 用户无路可走';
    await click('[data-sw="slack"]');
    const ok = await ev(() => document.querySelector('#overlay').classList.contains('open')
      && document.querySelector('#dlgTitle').textContent.includes('首次配置')
      && !!document.querySelector('.spinner'));
    await click('#dlgCancel');
    return ok || '空态下点开关没反应';
  });
  await t('空态演示还原', async () => { await click('#btnEmpty');
    const restored = await ev(() => document.querySelector('#emptyState').style.display === 'none'
      && !!document.querySelector('#grpSupported').offsetParent
      && document.querySelector('.vs-btn[data-view="clones"]').getAttribute('aria-selected') === 'true');
    if (!restored) return '空态没有还原，或还原后不在「分身」视图';
    /* 「添加应用分身」是「分身」视图底部的第二个入口：点了必须真的到「所有应用」 */
    await click('#rowPickApps');
    return await ev(() => !!document.querySelector('#viewAll').offsetParent
      && !!document.querySelector('#search')?.offsetParent) || '「添加应用分身」没有切到所有应用'; });

  // —— 横幅两种语义：致命不可自行解除，降级可关但不影响功能 ——
  await t('致命横幅 → 开关被拦截，且没有"知道了"按钮', async () => { await showView('all'); await click('#btnBanner');
    const blocked = await ev(() => document.body.classList.contains('blocked')
      && document.querySelector('#banner').classList.contains('show')
      && !document.querySelector('#banner button'));
    await click('[data-sw="firefox"]');
    const stillOff = await ev(() => !document.querySelector('[data-sw="firefox"]').checked
      && !document.querySelector('#overlay').classList.contains('open'));
    /* 致命横幅冻结的是"要 bwrap 才能成事"的操作，不是导航：切换视图必须仍然可用，
       否则标题栏上就是一个可见但点不动的控件（GTK 侧同样不碰 AdwViewSwitcher） */
    await click('.vs-btn[data-view="clones"]');
    const canSwitch = await ev(() => !!document.querySelector('#viewClones').offsetParent);
    await showView('all');
    /* 屏3 tab 也在工具栏里（被放行），但致命状态下它不能开出一个"按钮点不动"的探测框 */
    await click('[data-scr="3"]');
    const noGhost = await ev(() => !document.querySelector('#overlay').classList.contains('open')
      && !!document.querySelector('#scr1').offsetParent);
    return (blocked && stillOff && canSwitch && noGhost) || '致命态下开关没被拦、视图切不动或开出了探测框'; });
  /* 致命状态不能连"状态演示"一起冻住：环境恢复后界面若回不来，就是同一个死锁。
     不依赖上一条的结果——先按当前状态保证横幅是开着的，再关它 */
  await t('致命横幅（环境恢复）→ 横幅收起、开关重新响应、取消后开关回弹', async () => {
    if (!await ev(() => document.body.classList.contains('blocked'))) await click('#btnBanner');
    await click('#btnBanner');            // 环境恢复
    const cleared = await ev(() => !document.body.classList.contains('blocked')
      && !document.querySelector('#banner').classList.contains('show'));
    await click('[data-sw="firefox"]');   // 解除后开关必须重新响应（firefox 有档案 → open，直接进确认态）
    const live = await ev(() => document.querySelector('#overlay').classList.contains('open'));
    await click('#dlgCancel');
    /* 取消 = 什么都没发生：开关回弹到关，副标题仍说"已有隔离档案"（不是变成从未配过） */
    const rebound = await ev(() => { const s = document.querySelector('[data-sw="firefox"]');
      return !s.checked && s.closest('.row').querySelector('[data-sub]').textContent.includes('未开启 · 已有隔离档案'); });
    return cleared && live && rebound; });
  /* 只有 ELF 链接线索（没观测到实际调用）不足以替用户做决定：默认关，但组必须
     出现——没有单例组的应用也要能手动开启（这是用户唯一的入口） */
  await t('仅链接线索 → 默认关、无单例应用也有登录状态组', async () => {
    await click('[data-sw="firefox"]');
    const live = await ev(() => document.querySelector('#overlay').classList.contains('open'));
    if (!live) return 'firefox 没有进入确认态';
    const ok = await ev(() => {
      const kr = document.querySelector('[data-chk][data-kind="keyring"]');
      const g = document.querySelector('[data-group="keyring"]');
      return !!kr && !kr.checked && !kr.disabled
        && g.textContent.includes('可能使用系统密钥环')
        && !document.querySelector('[data-chk][data-kind="singleton"]');
    });
    await click('#dlgCancel');
    return ok || '链接线索被当成证据默认开启，或没有单例组时该组消失了';
  });
  await t('降级横幅 → 可"了解"关闭，且不影响开关', async () => { await click('#btnSoftBanner');
    const shown = await ev(() => document.querySelector('#bannerSoft').classList.contains('show')
      && !document.body.classList.contains('blocked'));
    await click('#bannerSoftDismiss');
    return shown && await ev(() => !document.querySelector('#bannerSoft').classList.contains('show')); });
  /* 「了解」只收起列表那条横幅，**不改变降级这个事实**：探测页里必须照旧说明"结果可能不完整"，
     否则用户在最需要知道这一点的时刻（确认勾选之前）看不到任何提示。
     原生里窗口顶部那条被 push 页遮住，所以探测页有自己的 AdwBanner */
  await t('降级横幅在探测页常驻（关掉列表那条也不消失）', async () => {
    await click('[data-sw="slack"]');
    /* 断言"可见"而不是"有 class"：class 在、CSS 却 display:none 的情况一样是没提示 */
    const shown = await ev(() => document.querySelector('#overlay').classList.contains('open')
      && !!document.querySelector('#dlgBanner').offsetParent
      && document.querySelector('#dlgBanner').textContent.includes('兼容模式'));
    await click('#dlgCancel');
    return shown || '探测页里看不到降级说明';
  });
  /* 取"第一行"是弱断言：`#unconfBox` 的首行可能是"没有匹配/全部已开启"提示行，
     点提示行当然不进详情。必须点一个**真实应用**行 */
  await t('未开启行点击行体 → 不进详情', async () => {
    return ev(() => {
      const row = document.querySelector('#unconfBox [data-sw="chrome"]')?.closest('.row');
      if (!row) return '找不到 chrome 行';
      row.querySelector('.row-title').click();
      return !!document.querySelector('#scr1').offsetParent && !document.querySelector('#scr2').offsetParent;
    }); });
  await t('取消探测 → tab 高亮跟实际可见屏（停在列表）', async () => {
    await click('[data-sw="chrome"]');            // 未支持 → 进探测
    await click('#dlgCancel');
    return ev(() => !!document.querySelector('#scr1').offsetParent
      && document.querySelector('[data-scr="1"]').getAttribute('aria-selected') === 'true');
  });

  /* 探测中点两次屏3 tab 会留下第二个定时器：旧实现里它 1.2s 后照样执行，
     把已关闭的对话框重绘并把 tab 高亮改成屏3（可见的却是屏2）。契约是"取消 = 什么都没发生" */
  await t('探测中重入屏3 → 取消后过期回调不改写 tab 高亮', async () => {
    await click('[data-scr="3"]');        // 开始探测 → 落到 demo 应用详情 + 对话框
    await click('[data-scr="3"]');        // 探测中重入，旧回调必须作废
    await click('#dlgCancel');
    await new Promise(r => setTimeout(r, 1500));   // 熬过探测完成时刻
    return ev(() => !document.querySelector('#overlay').classList.contains('open')
      && !!document.querySelector('#scr2').offsetParent
      && document.querySelector('[data-scr="2"]').getAttribute('aria-selected') === 'true'
      && document.querySelector('[data-scr="3"]').getAttribute('aria-selected') === 'false'); });

  // —— 屏3 失败态：零写入 → error，不生成空档案 ——
  await t('探测零写入 → 失败态，且不生成空档案', async () => {
    await click('[data-sw="chrome"]');
    const arrived = await waitFor(() => document.querySelector('#dlgConfirm').textContent === '重试');
    return arrived && await ev(() => document.querySelector('#dlgBody').textContent.includes('这次没有跟踪到任何写入')
      && !document.querySelector('[data-chk]')      // 没有可确认的路径
      && !document.querySelector('#dlgConfirm').disabled); });   // 「重试」必须点得动
  await t('失败态「重试」→ 回到探测中', async () => {
    await click('#dlgConfirm');
    const back = await ev(() => document.querySelector('#overlay').classList.contains('open')
      && !!document.querySelector('.spinner')
      && document.querySelector('#dlgConfirm').style.display === 'none'
      && document.querySelector('#dlgCancel').textContent.includes('取消探测'));
    await click('#dlgCancel');
    return back; });

  /* 闸门失败必须带**自己的原因**：「应用还开着」是最高频的首启失败，
     若它显示成"没有跟踪到任何写入"，用户会把闸门失败误诊成"这个应用不写盘"，然后反复重试 */
  await t('闸门：主实例还在运行 → 「还在运行」独立文案，且不启动探测会话', async () => {
    await click('[data-sw="obsidian"]');
    const ok = await ev(() => document.querySelector('#dlgTitle').textContent.includes('首次配置')
      && document.querySelector('#dlgBody').textContent.includes('还在运行')
      && !document.querySelector('#dlgBody').textContent.includes('没有跟踪到任何写入')
      && !document.querySelector('.spinner'));      // 连会话都没起
    await click('#dlgCancel');
    return ok || '闸门失败没走到独立文案';
  });
  /* 档案在、但记的路径已不存在（应用升级换了数据目录）：不能复用这份档案——
     复用 = 造一个什么都不隔离、启动即报错（--bind 源不存在）的分身 */
  await t('档案失效 → 入口是「重新配置」，副标题点名失效，且重跑探测', async () => {
    const subOk = await ev(() => document.querySelector('[data-sw="notion"]')
      .closest('.row').querySelector('[data-sub]').textContent.includes('档案已失效'));
    await click('[data-sw="notion"]');
    const headOk = await ev(() => document.querySelector('#dlgTitle').textContent.includes('重新配置')
      && !!document.querySelector('.spinner')
      && document.querySelector('#dlgCancel').textContent.includes('取消探测'));
    await click('#dlgCancel');
    return (subOk && headOk) || '副标题或入口标题不对';
  });

  // —— 开关：开 = 首次配置（无档案）/ 开启分身（有档案）；关 = 二次确认（运行中先拒绝）——
  await t('开关开(Slack) → 进入探测页（首次配置）', async () => { await click('[data-sw="slack"]');
    return ev(() => document.querySelector('#overlay').classList.contains('open')
              && document.querySelector('#dlgTitle').textContent.includes('首次配置')
              && !!document.querySelector('.spinner')); });
  await t('开关开启的探测确认 → 副标题变实例计数', async () => {
    await waitFor(() => !!document.querySelector('[data-chk]'));
    await confirmAndWait();
    return ev(() => { const s=document.querySelector('[data-sw="slack"]'); return s.checked
      && s.closest('.row').querySelector('[data-sub]').textContent.includes('1 个分身'); }); });
  /* 运行中「启动」换成「停止」（ADR-028）：隐藏一个点不动的按钮，同时让
     "能做什么"保持可见；删除仍禁用（rm 对运行中实例一律拒绝） */
  await t('运行中实例：显示「停止」，隐藏「启动」，删除禁用(Telegram)', async () => {
    await click('[data-app="telegram"]');
    return ev(() => !!document.querySelector('#instBox .runbadge')
              && !!document.querySelector('[data-stop="0"]')
              && !document.querySelector('[data-launch="0"]')
              && document.querySelector('[data-del="0"]').disabled); });
  /* 停止会结束应用，未保存内容可能丢：二次确认，默认是「取消」 */
  await t('停止分身 → 弹确认且默认「取消」，取消后仍在运行', async () => {
    await click('[data-stop="0"]');
    const ok = await ev(() => document.querySelector('#alertOverlay').classList.contains('open')
      && document.querySelector('#alertOk').textContent === '停止'
      && document.querySelector('#alertBody').textContent.includes('未保存')
      && document.querySelector('#alertCancel').dataset.default === '1');
    await click('#alertCancel');
    return (ok && await ev(() => !document.querySelector('#alertOverlay').classList.contains('open')
              && !!document.querySelector('#instBox .runbadge')))
      || '停止确认的默认响应不对，或取消后运行态被改动';
  });
  await t('停止确认 → 运行标记消失、启动按钮回来', async () => {
    await click('[data-stop="0"]'); await click('#alertOk');
    const cleaned = await ev(() => !document.querySelector('#instBox .runbadge')
      && !!document.querySelector('[data-launch="0"]')
      && !document.querySelector('[data-stop="0"]'));
    /* 后面的「关运行中实例的开关」还要用 Telegram 的运行态，还原演示数据 */
    await ev(() => { apps.find(a => a.id === 'telegram').instances[0].running = true;
                     renderDetail(); renderList(); });
    return cleaned || '停止后界面没有回到「启动」状态';
  });
  /* 运行数必须数出来，不能写死「1 个」：两个分身同时开着时说"1 个运行中"，
     用户会以为另一个挂了。断言走完整的 renderList 路径，而不是只测 subOf 这个辅助函数 */
  await t('副标题的运行数按实际计数（不是硬编码 1）', async () => {
    const txt = await ev(() => {
      const a = apps.find(x => x.id === 'wechat');
      a.instances.push({ id:'t1', display_name:'临时一', size:'1 MB', last:'刚刚', running:true },
                       { id:'t2', display_name:'临时二', size:'1 MB', last:'刚刚', running:true });
      renderList();
      const sub = document.querySelector('#supportedBox [data-app="wechat"] [data-sub]')?.textContent || '';
      a.instances.length = 0; renderList();      // 还原，别污染后续断言
      return sub;
    });
    return txt.includes('2 个运行中') || '副标题没按实际运行数渲染：' + txt;
  });
  /* 有档案且可创建的应用必须能走完「新建分身」；候选数据缺了，确认按钮会恒灰——
     那是一个"点了没反应"的死入口（门禁只数 config 项，而它一条都没有） */
  await t('有档案可创建的应用 → 「新建分身」确认按钮可用', async () => {
    await click('#rowAddInst');
    const ok = await ev(() => document.querySelector('#overlay').classList.contains('open')
      && document.querySelector('#dlgTitle').textContent.includes('新建分身')
      && !document.querySelector('#dlgConfirm').disabled);
    await click('#dlgCancel');
    return ok || '进了克隆入口但确认按钮是灰的（候选/档案数据缺失）';
  });
  await t('关运行中实例的开关 → 拒绝并回弹', async () => {
    await click('#btnBack'); await click('[data-sw="telegram"]');
    const opened = await ev(() => document.querySelector('#alertOverlay').classList.contains('open')
      && document.querySelector('#alertOk').textContent === '知道了');
    await click('#alertOk');
    return opened && await ev(() => document.querySelector('[data-sw="telegram"]').checked); });
  /* 有档案没实例 → 「开启分身」：复用档案、跳过探测（标题说「首次配置」会让用户白等一分钟） */
  await t('开关开(qq) → 标题「开启分身」且不重跑探测', async () => { await showView('all'); await click('[data-sw="qq"]');
    return ev(() => { const d = document.querySelector('#dlgTitle').textContent;
      return document.querySelector('#overlay').classList.contains('open')
        && d.includes('开启分身') && !d.includes('首次配置')
        && !document.querySelector('.spinner')          // 直接进确认态，不跑探测
        && !!document.querySelector('[data-chk]')
        && document.querySelector('#dlgConfirm').textContent === '开启分身'; }); });
  await t('ok 读证据 → 盲区文案 + no_home_reads 可见', async () => {
    return ev(() => {
      const ok = document.querySelector('[data-banner="reads-ok"]');
      const no = document.querySelector('#no-home-reads');
      return !!ok && ok.offsetParent !== null && ok.textContent.includes('open/openat')
        && !!no && no.offsetParent !== null && no.textContent.includes('没有观察到读取路径');
    });
  });
  await t('开启确认 → 开关打开且 1 个分身', async () => { await confirmAndWait();
    return ev(() => { const s=document.querySelector('[data-sw="qq"]'); return s.checked
      && s.closest('.row').querySelector('[data-sub]').textContent.includes('1 个分身'); }); });
  await t('开关关(qq) → 弹确认，取消后开关回弹', async () => {
    await click('[data-sw="qq"]');
    const opened = await ev(() => document.querySelector('#alertOverlay').classList.contains('open'));
    await click('#alertCancel');
    const back = await ev(() => document.querySelector('[data-sw="qq"]').checked);
    return opened && back; });
  await t('开关关(qq) → 确认后实例清空并回到所有应用', async () => {
    await click('[data-sw="qq"]'); await click('#alertOk');
    return ev(() => {
      const inSupported = !!document.querySelector('#supportedBox [data-sw="qq"]');
      const inAll = !!document.querySelector('#unconfBox [data-sw="qq"]');
      return !inSupported && inAll;
    }); });

  // —— 重命名（只改 display_name，id 不可变；32 rune 上限）——
  await t('qq 重开（复用档案）→ 详情重命名', async () => {
    await click('#btnBack');               // 回到屏1
    await showView('all');                 // qq 已无实例，开关在「所有应用」视图里
    await click('[data-sw="qq"]');
    await confirmAndWait();
    const onDetail = await ev(() => document.querySelector('#hbTitle').textContent === 'QQ'
      && !!document.querySelector('.iname'));
    if (!onDetail) return '确认后没有落在 qq 详情页';
    await page.click('.iname');
    await page.evaluate(() => { document.querySelector('.iname').textContent = '工作号'; });
    await page.click('#hbTitle');
    await new Promise(r=>setTimeout(r,200));
    return ev(() => document.querySelector('#toast').textContent.includes('已重命名为「工作号」')); });
  await t('重命名后实例名保持', async () => {
    return ev(() => document.querySelector('.iname')?.textContent === '工作号'); });
  await t('重命名超过 32 字符 → 拒绝并撤销', async () => {
    await page.click('.iname');
    await page.evaluate(() => { document.querySelector('.iname').textContent = '名'.repeat(33); });
    await page.click('#hbTitle');
    await new Promise(r=>setTimeout(r,200));
    return ev(() => document.querySelector('#toast').textContent.includes('最多 32 个字符')
      && document.querySelector('.iname').textContent === '工作号'); });
  /* 第四个入口：重新探测。只更新档案，**不建实例、不动既有分身数据** */
  await t('重新探测 → 标题「重新探测」、按钮「保存档案」、实例数不变', async () => {
    const before = await ev(() => document.querySelectorAll('.iname').length);
    await click('#btnRediscover');
    const head = await ev(() => document.querySelector('#dlgTitle').textContent.includes('重新探测')
      && !!document.querySelector('.spinner')
      && document.querySelector('#dlgCancel').textContent.includes('取消探测'));
    if (!head) return '标题或探测中态不对：' + await textOf('#dlgTitle');
    await waitFor(() => !!document.querySelector('[data-chk]'));
    /* 重新探测只写档案、不建实例：fork 无处落地，不能给一个开了也不生效的开关 */
    if (await ev(() => !!document.querySelector('[data-fork]'))) return '重新探测不该出现数据来源开关';
    const label = await textOf('#dlgConfirm');
    if (label !== '保存档案') return '确认按钮应为「保存档案」，实际：' + label;
    await click('#dlgConfirm');
    const after = await ev(() => document.querySelectorAll('.iname').length);
    return after === before
      && await ev(() => document.querySelector('#toast').textContent.includes('档案已更新')); });
  await t('删中间实例后新建 → 显示名不重复', async () => {
    await click('#rowAddInst');                    // 再建一个：应有「分身 1」
    await confirmAndWait();
    await click('[data-del="0"]'); await click('#alertOk');   // 删掉「工作号」
    await click('#rowAddInst'); await confirmAndWait();        // 再建：不能又叫「分身 1」
    return ev(() => {
      const names = [...document.querySelectorAll('.iname')].map(e => e.textContent.trim());
      return names.length === 2 && new Set(names).size === 2;
    }); });

  /* 同应用内显示名唯一：GNOME 没有角标，Name 后缀是唯一区分手段 */
  await t('重命名撞名 → 拒绝并还原', async () => {
    const names = await ev(() => [...document.querySelectorAll('.iname')].map(e => e.textContent.trim()));
    if (names.length < 2) return '实例不足两个，无法测撞名';
    await page.click('.iname');
    await page.evaluate(n => {
      const el = document.querySelectorAll('.iname')[1];
      el.textContent = n;
      el.dispatchEvent(new FocusEvent('focusout', {bubbles:true}));
    }, names[0]);
    await new Promise(r => setTimeout(r, 200));
    return ev(() => {
      const now = [...document.querySelectorAll('.iname')].map(e => e.textContent.trim());
      return new Set(now).size === now.length
        && document.querySelector('#toast').textContent.includes('同名');
    }) || '撞名被接受了：' + await textOf('#toast');
  });

  /* 读证据不可用且零勾选：不直接报 no_writes，进入可补选的降级确认页；
     确认按钮初始禁用，勾选一条后启用（保存前仍要求 paths>0） */
  await t('读证据不可用 → confirm-empty，勾选后可继续', async () => {
    await click('#btnBack');
    await showView('all');
    await click('[data-sw="zcode"]');
    const arrived = await waitFor(() => document.querySelector('#dlgBody')?.dataset.state === 'confirm-empty');
    if (!arrived) return '没进入 confirm-empty';
    const banner = await ev(() => {
      const el = document.querySelector('[data-banner="reads-unavailable"]');
      return !!el && el.offsetParent !== null && el.textContent.includes('登录态可能未覆盖');
    });
    const disabled = await ev(() => document.querySelector('#dlgConfirm').disabled);
    await ev(() => document.querySelector('[data-chk][data-kind="public"]').click());
    const publicStillDisabled = await ev(() => document.querySelector('#dlgConfirm').disabled);
    await ev(() => document.querySelector('[data-chk][data-kind="config"]').click());
    const enabled = await ev(() => !document.querySelector('#dlgConfirm').disabled);
    await click('#dlgCancel');
    return (banner && disabled && publicStillDisabled && enabled) || 'banner/门禁状态不对';
  });

  /* 没有 gnome-keyring-daemon / dbus-send 时不能给一个"拨了也不生效"的开关：
     禁用并写清原因；已经开着时仍允许关闭（由 GTK 侧断言） */
  await t('能力缺失 → 登录状态行禁用并说明原因', async () => {
    await showView('all');
    await click('[data-sw="zcode"]');
    const arrived = await waitFor(() => !!document.querySelector('[data-chk][data-kind="keyring"]'), 5000);
    if (!arrived) return '没进入确认态';
    const ok = await ev(() => {
      const kr = document.querySelector('[data-chk][data-kind="keyring"]');
      return kr.disabled && !kr.checked
        && document.querySelector('[data-group="keyring"]').textContent.includes('未找到 gnome-keyring-daemon');
    });
    await click('#dlgCancel');
    return ok || '能力缺失时开关仍可拨，或没有说明原因';
  });
  await t('旧档案（read_trace=""）→ 提示重新探测可获取读证据', async () => {
    await click('[data-sw="notion"]');
    const arrived = await waitFor(() => !!document.querySelector('[data-banner="reads-legacy"]'));
    const ok = arrived && await ev(() => {
      const el = document.querySelector('[data-banner="reads-legacy"]');
      return el.offsetParent !== null && el.textContent.includes('旧档案');
    });
    await click('#dlgCancel');
    return ok || '旧档案没有独立文案';
  });

  /* 升级后私有总线不再激活宿主 keyring：只有"已开私有总线、未开独立密钥环"的
     旧档案会看到这句；没有它就等于让用户自己去发现登录态没跟上 */
  await t('旧档案（私有总线 + 未开密钥环）→ 迁移提示可见', async () => {
    await showView('all');
    await click('[data-sw="notion"]');
    const arrived = await waitFor(() => !!document.querySelector('[data-group="keyring"]'), 4000);
    if (!arrived) return '没进入确认态';
    const ok = await ev(() => document.querySelector('[data-group="keyring"]')
      .textContent.includes('升级后该分身不再使用宿主密钥环'));
    await click('#dlgCancel');
    return ok || '旧档案没有迁移提示';
  });

  console.log(`\n结果: ${pass} 通过 / ${fail} 失败`);
  console.log(errors.length ? '❗JS错误:\n' + errors.join('\n') : '全程无页面 JS 错误');
  await browser.close();
  // 退出码是闸门：非 0 即视为设计基线被破坏
  process.exitCode = (fail === 0 && errors.length === 0 && pass === EXPECTED) ? 0 : 1;
})();
